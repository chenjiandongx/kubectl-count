package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"k8s.io/klog/v2"
)

const (
	resyncPeriod = time.Minute * 5
	version      = "0.2.2"
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
	GroupVersion string `json:"groupVersion" yaml:"groupVersion"`
	Kind         string `json:"kind" yaml:"kind"`
	Count        int    `json:"count" yaml:"count"`
}

type IDMap struct {
	lock sync.Mutex
	m    map[string]map[string]int
	ids  []string
}

func NewIDMap() *IDMap {
	return &IDMap{
		m: map[string]map[string]int{},
	}
}

func (idm *IDMap) KindGroupVersion(id string) (string, string) {
	parts := strings.Split(id, "+")
	return parts[0], parts[1]
}

func (idm *IDMap) Add(id, namespace string) {
	idm.lock.Lock()
	defer idm.lock.Unlock()

	if _, ok := idm.m[id]; !ok {
		idm.m[id] = map[string]int{}
	}
	idm.m[id][namespace]++
}

func (idm *IDMap) Del(id, namespace string) {
	idm.lock.Lock()
	defer idm.lock.Unlock()

	if _, ok := idm.m[id]; !ok {
		return
	}
	idm.m[id][namespace]--
}

func (idm *IDMap) AddID(id string) {
	idm.ids = append(idm.ids, id)
}

func (idm *IDMap) GetRecords(order string, allNamespace bool) []Record {
	idm.lock.Lock()
	defer idm.lock.Unlock()

	records := map[string][]Record{}
	for id, counter := range idm.m {
		kind, groupVersion := idm.KindGroupVersion(id)
		rs := make([]Record, 0)
		for ns, c := range counter {
			rs = append(rs, Record{
				Namespace:    ns,
				Kind:         kind,
				GroupVersion: groupVersion,
				Count:        c,
			})
		}
		records[id] = rs
	}

	tmp := map[string][]Record{}
	if allNamespace {
		for id, counter := range records {
			kind, _ := idm.KindGroupVersion(id)
			r := Record{Kind: kind}
			for _, c := range counter {
				r.Count += c.Count
				r.GroupVersion = c.GroupVersion
			}
			tmp[id] = []Record{r}
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
	for _, id := range idm.ids {
		ret = append(ret, records[id]...)
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

func (cc *CounterController) list(s string) (*IDMap, error) {
	kinds := cc.sanitizeKinds(s)
	if len(kinds) == 0 {
		return nil, fmt.Errorf("invalid input kind name: '%s'", s)
	}

	apiResources, err := cc.getApiResources()
	if err != nil {
		return nil, err
	}

	idMap := NewIDMap()
	informers := map[string]cache.SharedIndexInformer{}
	for _, kind := range kinds {
		if ars, ok := apiResources[kind]; ok {
			for _, ar := range ars {
				informers[ar.ID()] = cc.factory.ForResource(schema.GroupVersionResource{
					Group:    ar.resource.Group,
					Version:  ar.resource.Version,
					Resource: ar.resource.Name,
				}).Informer()
				idMap.AddID(ar.ID())
			}
		}
	}

	if len(informers) == 0 {
		return nil, errors.New("no available informers found")
	}

	for id, informer := range informers {
		cloned := id
		informer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {})
		informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				o, ok := obj.(*unstructured.Unstructured)
				if !ok {
					return
				}
				idMap.Add(cloned, o.GetNamespace())
			},
			DeleteFunc: func(obj interface{}) {
				o, ok := obj.(*unstructured.Unstructured)
				if !ok {
					return
				}
				idMap.Del(cloned, o.GetNamespace())
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
	return idMap, nil
}

type APIResourceGV struct {
	resource     v1.APIResource
	groupVersion string
}

func (agv APIResourceGV) ID() string {
	return agv.resource.Kind + "+" + agv.groupVersion
}

func (cc *CounterController) getApiResources() (map[string][]APIResourceGV, error) {
	resources, _ := cc.discoveryClient.ServerPreferredResources()
	rm := make(map[string][]APIResourceGV)
	for _, resource := range resources {
		gv, err := schema.ParseGroupVersion(resource.GroupVersion)
		if err != nil {
			return nil, err
		}

		for _, r := range resource.APIResources {
			cloned := r
			cloned.Group = gv.Group
			cloned.Version = gv.Version
			agv := APIResourceGV{resource: cloned, groupVersion: resource.GroupVersion}

			keys := []string{r.Name, strings.ToLower(r.Kind), fmt.Sprintf("%s.%s", r.Name, gv.Group)}
			for _, key := range keys {
				rm[key] = append(rm[key], agv)
			}

			for _, shortName := range r.ShortNames {
				rm[shortName] = append(rm[shortName], agv)
			}
			if r.SingularName != "" {
				rm[r.SingularName] = append(rm[r.SingularName], agv)
			}
		}
	}

	return rm, nil
}

func (cc *CounterController) tableRender(records []Record) {
	headers := []string{"Namespace", "GroupVersion", "Kind", "Count"}
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader(headers)
	table.SetAutoFormatHeaders(false)
	table.SetAutoMergeCells(true)
	table.SetRowLine(true)

	for _, record := range records {
		table.Append([]string{record.Namespace, record.GroupVersion, record.Kind, strconv.Itoa(record.Count)})
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
	idMap, err := cc.list(kinds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Oh...] Failed to list resources, error: %v", err)
		os.Exit(1)
	}

	records := idMap.GetRecords(order, allNamespace)
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
