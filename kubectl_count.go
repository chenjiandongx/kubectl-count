package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"k8s.io/klog/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/cache"
)

const (
	resyncPeriod = time.Minute * 5
	version      = "v0.1.0"
)

var cf = genericclioptions.NewConfigFlags(true)

var rootCmd *cobra.Command

func init() {
	rootCmd = &cobra.Command{
		Use:   "kubectl-count <kinds>",
		Short: "Show resources count in the cluster.",
		Example: `  # display a table of specified resources count, resources split by comma.
  kubectl count pods,ds,deploy

  # display kube-system cluster count info in yaml format.
  kubectl count -oy -n kube-system rs,ep`,
		Version: version,
		Args:    cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			klog.SetOutput(io.Discard)
			klog.LogToStderr(false)

			namespace, _ := cmd.Flags().GetString("namespace")
			ctr, err := NewCounterController(namespace)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[Oh...] Failed to Create Controller, error: %v", err)
				os.Exit(1)
			}

			kinds := args[0]
			order, _ := cmd.Flags().GetString("order")
			format, _ := cmd.Flags().GetString("output-format")
			allNamespace, _ := cmd.Flags().GetBool("all-namespaces")
			ctr.Render(kinds, order, format, allNamespace)
		},
	}

	rootCmd.Flags().BoolP("all-namespaces", "A", false, "if present, resources aggregated by all namespaces")
	rootCmd.Flags().StringP("order", "O", "asc", "used to sort the counts in ascending or descending order. [asc(a)|desc(d)]")
	rootCmd.Flags().StringP("output-format", "o", "table", "output format. [json(j)|table(t)|yaml(y)]")
	cf.AddFlags(rootCmd.Flags())
}

type Record struct {
	Namespace    string `json:"namespace" yaml:"namespace"`
	Kind         string `json:"kind" yaml:"kind"`
	GroupVersion string `json:"groupVersion" yaml:"groupVersion"`
	Count        int    `json:"count" yaml:"count"`
}

type KindNamespaceMap struct {
	lock  sync.Mutex
	m     map[string]map[string]int
	kinds []string
	gvs   map[string]string
}

func NewKindNamespaceMap() *KindNamespaceMap {
	return &KindNamespaceMap{
		m:   map[string]map[string]int{},
		gvs: map[string]string{},
	}
}

func (kn *KindNamespaceMap) Add(kind, namespace string) {
	kn.lock.Lock()
	defer kn.lock.Unlock()

	if _, ok := kn.m[kind]; !ok {
		kn.m[kind] = map[string]int{}
	}
	kn.m[kind][namespace]++
}

func (kn *KindNamespaceMap) Del(kind, namespace string) {
	kn.lock.Lock()
	defer kn.lock.Unlock()

	if _, ok := kn.m[kind]; !ok {
		return
	}
	kn.m[kind][namespace]--
}

func (kn *KindNamespaceMap) AddKind(k, gv string) {
	kn.lock.Lock()
	defer kn.lock.Unlock()

	kn.kinds = append(kn.kinds, k)
	kn.gvs[k] = gv
}

func (kn *KindNamespaceMap) GetRecords(order string, allNamespace bool) []Record {
	kn.lock.Lock()
	defer kn.lock.Unlock()

	records := map[string][]Record{}
	for kind, counter := range kn.m {
		rs := make([]Record, 0)
		for ns, c := range counter {
			rs = append(rs, Record{
				Namespace:    ns,
				Kind:         kind,
				GroupVersion: kn.gvs[kind],
				Count:        c,
			})
		}
		records[kind] = rs
	}

	tmp := map[string][]Record{}
	if allNamespace {
		for kind, counter := range records {
			r := Record{Kind: kind}
			for _, c := range counter {
				r.Count += c.Count
				r.GroupVersion = c.GroupVersion
			}
			tmp[kind] = []Record{r}
		}
		records = tmp
	}

	order = strings.ToLower(order)
	switch order {
	case "desc", "d":
		for _, rs := range records {
			sort.Slice(rs, func(i, j int) bool {
				return rs[i].Count > rs[j].Count
			})
		}
	default:
		for _, rs := range records {
			sort.Slice(rs, func(i, j int) bool {
				return rs[i].Count < rs[j].Count
			})
		}
	}

	ret := make([]Record, 0)
	for _, k := range kn.kinds {
		ret = append(ret, records[k]...)
	}
	return ret
}

type CounterController struct {
	ctx             context.Context
	cancel          context.CancelFunc
	discoveryClient discovery.CachedDiscoveryInterface
	factory         dynamicinformer.DynamicSharedInformerFactory
}

func NewCounterController(namespace string) (*CounterController, error) {
	restConfig, err := cf.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	restConfig.QPS = 1000
	restConfig.Burst = 1000

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	dc, err := cf.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &CounterController{
		ctx:             ctx,
		cancel:          cancel,
		discoveryClient: dc,
		factory:         dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, resyncPeriod, namespace, nil),
	}, nil
}

func (cc *CounterController) sanitizeKinds(s string) []string {
	var kinds []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			kinds = append(kinds, part)
		}
	}
	return kinds
}

func (cc *CounterController) list(s string) (*KindNamespaceMap, error) {
	kinds := cc.sanitizeKinds(s)
	if len(kinds) == 0 {
		return nil, fmt.Errorf("invalid input kind name: '%s'", s)
	}

	apiResources, gvs, err := cc.getApiResources()
	if err != nil {
		return nil, err
	}

	kindNamespaceMap := NewKindNamespaceMap()
	informers := map[string]cache.SharedIndexInformer{}
	for _, kind := range kinds {
		if ars, ok := apiResources[kind]; ok {
			for _, ar := range ars {
				informers[ar.Kind] = cc.factory.ForResource(schema.GroupVersionResource{
					Group:    ar.Group,
					Version:  ar.Version,
					Resource: ar.Name,
				}).Informer()
				kindNamespaceMap.AddKind(ar.Kind, gvs[ar.Kind])
			}
		}
	}

	if len(informers) == 0 {
		return nil, errors.New("no available informers found")
	}

	for kind, informer := range informers {
		k := kind
		informer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {})
		informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				o, ok := obj.(*unstructured.Unstructured)
				if !ok {
					return
				}
				kindNamespaceMap.Add(k, o.GetNamespace())
			},
			DeleteFunc: func(obj interface{}) {
				o, ok := obj.(*unstructured.Unstructured)
				if !ok {
					return
				}
				kindNamespaceMap.Del(k, o.GetNamespace())
			},
		})
		go informer.Run(cc.ctx.Done())
	}

	for kind, informer := range informers {
		if !cache.WaitForNamedCacheSync(kind, cc.ctx.Done(), informer.HasSynced) {
			return nil, fmt.Errorf("failed to sync %s cache", kind)
		}
	}

	cc.cancel()
	return kindNamespaceMap, nil
}

func (cc *CounterController) getApiResources() (map[string][]v1.APIResource, map[string]string, error) {
	resources, _ := cc.discoveryClient.ServerPreferredResources()
	rm := make(map[string][]v1.APIResource)
	gvs := make(map[string]string)
	for _, resource := range resources {
		gv, err := schema.ParseGroupVersion(resource.GroupVersion)
		if err != nil {
			return nil, nil, err
		}

		for _, r := range resource.APIResources {
			cloned := r
			cloned.Group = gv.Group
			cloned.Version = gv.Version
			gvs[r.Kind] = resource.GroupVersion

			keys := []string{r.Name, strings.ToLower(r.Kind), fmt.Sprintf("%s.%s", r.Name, gv.Group)}
			for _, key := range keys {
				rm[key] = append(rm[key], cloned)
			}

			for _, shortName := range r.ShortNames {
				rm[shortName] = append(rm[shortName], cloned)
			}
			if r.SingularName != "" {
				rm[r.SingularName] = append(rm[r.SingularName], cloned)
			}
		}
	}

	return rm, gvs, nil
}

func (cc *CounterController) tableRender(records []Record) {
	headers := []string{"Namespace", "Kind", "GroupVersion", "Count"}
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader(headers)
	table.SetAutoFormatHeaders(false)
	table.SetAutoMergeCells(true)
	table.SetRowLine(true)

	for _, record := range records {
		table.Append([]string{record.Namespace, record.Kind, record.GroupVersion, strconv.Itoa(record.Count)})
	}
	table.Render()
}

func (cc *CounterController) jsonRender(records []Record) {
	b, err := json.MarshalIndent(records, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Oh...] Failed to marshal JSON data, error: %v", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}

func (cc *CounterController) yamlRender(records []Record) {
	b, err := yaml.Marshal(records)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Oh...] Failed to marshal YAML data, error: %v", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}

func (cc *CounterController) Render(kinds, order, output string, allNamespace bool) {
	kn, err := cc.list(kinds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Oh...] Failed to list resources, error: %v", err)
		os.Exit(1)
	}

	records := kn.GetRecords(order, allNamespace)
	if len(records) <= 0 {
		fmt.Fprintln(os.Stdout, "[Oh...] No Resources found!")
		os.Exit(1)
	}

	switch output {
	case "json", "j":
		cc.jsonRender(records)
	case "yaml", "y":
		cc.yamlRender(records)
	default:
		cc.tableRender(records)
	}
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "[Oh...] Failed to exec command: %v", err)
	}
}
