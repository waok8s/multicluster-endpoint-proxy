/*
 */

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
	waoendpointproxyv1beta1 "github.com/waok8s/wao-endpoint-proxy/api/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type DistributedRequest struct {
	cache.Cache
	client.Client
	wao *Wao
}

//#+kubebuilder:rbac:groups=waoendpointproxy.bitmedia.co.jp,resources=*,verbs=get;list

const (
	REQ_TYPE_ONLY_MY_DOMAIN = iota
	REQ_TYPE_COPY_ALL_DOMAIN
	REQ_TYPE_MODIFY
)

const (
	RESP_KIND_NO_MERGE = iota
	RESP_KIND_TABLE
	RESP_KIND_ANY_LIST
)

const (
	API_TYPE_OTHER = iota
	API_TYPE_API
	API_TYPE_APIS
	API_TYPE_CORE_V1_NAMESPACE
	API_TYPE_CORE_V1_POD
	API_TYPE_CORE_V1_SERVICE
	API_TYPE_CORE_V1_PERSISTENTVOLUME
	API_TYPE_CORE_V1_PERSISTENTVOLUMECLAIM
	API_TYPE_APPS_V1_DEPLOYMENT
	API_TYPE_APPS_V1_STATEFULSET
	API_TYPE_APIS_CLUSTERSCORE
)

type CommonResource[T1 any, T2 any] struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	// +optional
	Spec T1 `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	// Read-only.
	// +optional
	Status T2 `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

type AnyList[T any] struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []T `json:"items"`
}

type Deploy[T1 appsv1.DeploymentSpec | appsv1.StatefulSetSpec, T2 any] struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	// +optional
	Spec T1 `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	// +optional
	Status T2 `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

func NewDistributedRequest(mgr ctrl.Manager) *DistributedRequest {
	return &DistributedRequest{
		Cache:  mgr.GetCache(),
		Client: mgr.GetClient(),
		wao:    NewWao(mgr),
	}
}

type WaoEndpointRequest struct {
	Host    string
	Request *http.Request
}

type GetApiTypeMasterProp struct {
	expr    string
	apiType int
}

var getApiTypeMaster = []GetApiTypeMasterProp{
	{expr: `^/api$`, apiType: API_TYPE_API},
	{expr: `^/apis$`, apiType: API_TYPE_APIS},
	{expr: `^/api/v1/namespaces$`, apiType: API_TYPE_CORE_V1_NAMESPACE},
	{expr: `^/api/v1/namespaces/[^/]*$`, apiType: API_TYPE_CORE_V1_NAMESPACE},
	{expr: `^/api/v1/namespaces/[^/]*/pods$`, apiType: API_TYPE_CORE_V1_POD},
	{expr: `^/api/v1/namespaces/[^/]*/pods/.*$`, apiType: API_TYPE_CORE_V1_POD},
	{expr: `^/api/v1/namespaces/[^/]*/services$`, apiType: API_TYPE_CORE_V1_SERVICE},
	{expr: `^/api/v1/namespaces/[^/]*/services/[^/]*$`, apiType: API_TYPE_CORE_V1_SERVICE},
	{expr: `^/api/v1/persistentvolumes$`, apiType: API_TYPE_CORE_V1_PERSISTENTVOLUME},
	{expr: `^/api/v1/persistentvolumes/[^/]*$`, apiType: API_TYPE_CORE_V1_PERSISTENTVOLUME},
	{expr: `^/api/v1/namespaces/[^/]*/persistentvolumeclaims$`, apiType: API_TYPE_CORE_V1_PERSISTENTVOLUMECLAIM},
	{expr: `^/api/v1/namespaces/[^/]*/persistentvolumeclaims/[^/]*$`, apiType: API_TYPE_CORE_V1_PERSISTENTVOLUMECLAIM},
	{expr: `^/apis/apps/v1/namespaces/[^/]*/deployments$`, apiType: API_TYPE_APPS_V1_DEPLOYMENT},
	{expr: `^/apis/apps/v1/namespaces/[^/]*/deployments/[^/]*/scale$`, apiType: API_TYPE_APPS_V1_DEPLOYMENT},
	{expr: `^/apis/apps/v1/namespaces/[^/]*/deployments/[^/]*$`, apiType: API_TYPE_APPS_V1_DEPLOYMENT},
	{expr: `^/apis/apps/v1/namespaces/[^/]*/statefulsets$`, apiType: API_TYPE_APPS_V1_STATEFULSET},
	{expr: `^/apis/apps/v1/namespaces/[^/]*/statefulsets/[^/]*/scale$`, apiType: API_TYPE_APPS_V1_STATEFULSET},
	{expr: `^/apis/apps/v1/namespaces/[^/]*/statefulsets/[^/]*$`, apiType: API_TYPE_APPS_V1_STATEFULSET},
	{expr: `^/apis/waoendpointproxy.bitmedia.co.jp/v1beta1/clusterscores$`, apiType: API_TYPE_APIS_CLUSTERSCORE},
	{expr: `^/apis/waoendpointproxy.bitmedia.co.jp/v1beta1/namespaces/[^/]*c/lusterscores$`, apiType: API_TYPE_APIS_CLUSTERSCORE},
}

func GetApiType(path string) (apiType int) {
	apiType = API_TYPE_OTHER
	for _, pro := range getApiTypeMaster {
		re, _ := regexp.Compile(pro.expr)
		if re.MatchString(path) {
			apiType = pro.apiType
			break
		}
	}
	return
}

func SplitReplicas(items []waoendpointproxyv1beta1.ClusterScore, replicas int64) (result []int64) {
	if replicas < 0 {
		result = nil
		return
	}
	var tmpTotalScore int64
	for _, c := range items {
		if c.Status.Score > 0 {
			tmpTotalScore += c.Status.Score
		}
	}
	var totalScore int64
	var scores []int64
	for _, c := range items {
		if c.Status.Score > 0 {
			score := tmpTotalScore - c.Status.Score
			scores = append(scores, score)
			totalScore += score
		} else {
			scores = append(scores, c.Status.Score)
		}
	}
	var total int64
	for i, score := range scores {
		var r int64
		if replicas > 0 {
			if i == 0 {
				if score > 0 {
					r = int64(math.Ceil(float64(replicas) * float64(score) / float64(totalScore)))
				}
				if r == 0 {
					r = 1
				}
			} else {
				if score > 0 {
					r = int64(math.Round(float64(replicas) * float64(score) / float64(totalScore)))
				}
			}
			if (total + r) > replicas {
				r = (replicas - total)
			}
			total += r
		}
		result = append(result, r)
	}
	return
}

func (dr *DistributedRequest) CreateDistributedRequest(req *http.Request, reqBody []byte) (responseKind int, requests []WaoEndpointRequest) {
	var items []waoendpointproxyv1beta1.ClusterScore
	clusterScores := waoendpointproxyv1beta1.ClusterScoreList{}
	dr.Cache.List(context.Background(), &clusterScores)
	for i, clusterScore := range clusterScores.Items {
		//lint:ignore S1002 set true in SetupWithManager
		if clusterScore.Spec.OwnCluster == true && i > 0 {
			items = append([]waoendpointproxyv1beta1.ClusterScore{clusterScore}, items...)
		} else {
			items = append(items, clusterScore)
		}
	}

	LABEL_KEY := os.Getenv("LABEL_KEY")
	LABEL_VALUE_MY_DOMAIN := os.Getenv("LABEL_VALUE_MY_DOMAIN")

	apiType := API_TYPE_OTHER
	reqType := REQ_TYPE_ONLY_MY_DOMAIN

	if len(items) > 0 {
		switch req.Method {
		case http.MethodGet:
			switch GetApiType(req.URL.Path) {
			case API_TYPE_API, API_TYPE_APIS, API_TYPE_APIS_CLUSTERSCORE:
			default:
				accept := req.Header.Get("accept")
				re, _ := regexp.Compile(`(?i)as=APIGroupDiscoveryList`)
				if !re.MatchString(accept) {
					re, _ = regexp.Compile(`(?i)as=Table`)
					if re.MatchString(accept) {
						reqType = REQ_TYPE_COPY_ALL_DOMAIN
						responseKind = RESP_KIND_TABLE
					} else {
						reqType = REQ_TYPE_COPY_ALL_DOMAIN
						responseKind = RESP_KIND_ANY_LIST
					}
				}
			}
		case http.MethodPost, http.MethodPut:
			apiType = GetApiType(req.URL.Path)
			switch apiType {
			case API_TYPE_CORE_V1_NAMESPACE, API_TYPE_CORE_V1_SERVICE, API_TYPE_CORE_V1_PERSISTENTVOLUME, API_TYPE_CORE_V1_PERSISTENTVOLUMECLAIM:
				for i, c := range items {
					val := c.Spec.Region
					if i == 0 {
						val = LABEL_VALUE_MY_DOMAIN
					}
					var resource CommonResource[any, any]
					json.Unmarshal(reqBody, &resource)
					if resource.ObjectMeta.Labels == nil {
						resource.ObjectMeta.Labels = map[string]string{LABEL_KEY: val}
					} else {
						resource.ObjectMeta.Labels[LABEL_KEY] = val
					}
					s, _ := json.Marshal(resource)
					u, _ := url.Parse(c.Spec.Endpoint)
					newReq := req.Clone(context.Background())
					newReq.Header = CloneHeader(req.Header)
					newReq.Host = u.Host
					newReq.ContentLength = int64(len(s))
					newReq.Body = io.NopCloser(bytes.NewReader(s))
					requests = append(requests, WaoEndpointRequest{Host: c.Spec.Endpoint, Request: newReq})
				}
				reqType = REQ_TYPE_MODIFY
			case API_TYPE_APPS_V1_DEPLOYMENT:
				reqType = REQ_TYPE_MODIFY
				var replicas int64 = 1
				reqBodyStr := *(*string)(unsafe.Pointer(&reqBody))
				result := gjson.Get(reqBodyStr, "spec.replicas")
				if result.Index > 0 {
					replicas = result.Int()
				}
				replicaNum := SplitReplicas(items, replicas)
				for i, c := range items {
					val := c.Spec.Region
					if i == 0 {
						val = LABEL_VALUE_MY_DOMAIN
					}
					var deploy appsv1.Deployment
					json.Unmarshal(reqBody, &deploy)
					if deploy.ObjectMeta.Labels == nil {
						deploy.ObjectMeta.Labels = map[string]string{LABEL_KEY: val}
					} else {
						deploy.ObjectMeta.Labels[LABEL_KEY] = val
					}
					if deploy.Spec.Template.ObjectMeta.Labels == nil {
						deploy.Spec.Template.ObjectMeta.Labels = map[string]string{LABEL_KEY: val}
					} else {
						deploy.Spec.Template.ObjectMeta.Labels[LABEL_KEY] = val
					}
					replica := int32(replicaNum[i])
					deploy.Spec.Replicas = &replica
					s, _ := json.Marshal(deploy)
					u, _ := url.Parse(c.Spec.Endpoint)
					newReq := req.Clone(context.Background())
					newReq.Header = CloneHeader(req.Header)
					newReq.Host = u.Host
					newReq.ContentLength = int64(len(s))
					newReq.Body = io.NopCloser(bytes.NewReader(s))
					requests = append(requests, WaoEndpointRequest{Host: c.Spec.Endpoint, Request: newReq})
				}
			case API_TYPE_APPS_V1_STATEFULSET:
				reqType = REQ_TYPE_MODIFY
				var replicas int64 = 1
				reqBodyStr := *(*string)(unsafe.Pointer(&reqBody))
				result := gjson.Get(reqBodyStr, "spec.replicas")
				if result.Index > 0 {
					replicas = result.Int()
				}
				replicaNum := SplitReplicas(items, replicas)
				for i, c := range items {
					val := c.Spec.Region
					if i == 0 {
						val = LABEL_VALUE_MY_DOMAIN
					}
					var stateful appsv1.StatefulSet
					json.Unmarshal(reqBody, &stateful)
					if stateful.ObjectMeta.Labels == nil {
						stateful.ObjectMeta.Labels = map[string]string{LABEL_KEY: val}
					} else {
						stateful.ObjectMeta.Labels[LABEL_KEY] = val
					}
					if stateful.Spec.Template.ObjectMeta.Labels == nil {
						stateful.Spec.Template.ObjectMeta.Labels = map[string]string{LABEL_KEY: val}
					} else {
						stateful.Spec.Template.ObjectMeta.Labels[LABEL_KEY] = val
					}
					replica := int32(replicaNum[i])
					stateful.Spec.Replicas = &replica
					s, _ := json.Marshal(stateful)
					u, _ := url.Parse(c.Spec.Endpoint)
					newReq := req.Clone(context.Background())
					newReq.Header = CloneHeader(req.Header)
					newReq.Host = u.Host
					newReq.ContentLength = int64(len(s))
					newReq.Body = io.NopCloser(bytes.NewReader(s))
					requests = append(requests, WaoEndpointRequest{Host: c.Spec.Endpoint, Request: newReq})
				}
			default:
				reqType = REQ_TYPE_ONLY_MY_DOMAIN
			}
		case http.MethodDelete:
			switch GetApiType(req.URL.Path) {
			case API_TYPE_CORE_V1_NAMESPACE, API_TYPE_CORE_V1_POD, API_TYPE_CORE_V1_PERSISTENTVOLUME, API_TYPE_CORE_V1_PERSISTENTVOLUMECLAIM, API_TYPE_APPS_V1_DEPLOYMENT, API_TYPE_APPS_V1_STATEFULSET:
				reqType = REQ_TYPE_COPY_ALL_DOMAIN
			default:
				reqType = REQ_TYPE_ONLY_MY_DOMAIN
			}
		case http.MethodHead:
		case http.MethodPatch:
			apiType = GetApiType(req.URL.Path)
			switch apiType {
			case API_TYPE_CORE_V1_NAMESPACE, API_TYPE_CORE_V1_SERVICE, API_TYPE_CORE_V1_PERSISTENTVOLUME, API_TYPE_CORE_V1_PERSISTENTVOLUMECLAIM:
				reqType = REQ_TYPE_COPY_ALL_DOMAIN
			case API_TYPE_APPS_V1_DEPLOYMENT, API_TYPE_APPS_V1_STATEFULSET:
				reqBodyStr := *(*string)(unsafe.Pointer(&reqBody))
				result := gjson.Get(reqBodyStr, "spec.replicas")
				if result.Index > 0 {
					reqType = REQ_TYPE_MODIFY
					replicas := result.Int()
					replicaNum := SplitReplicas(items, replicas)
					for i, c := range items {
						u, _ := url.Parse(c.Spec.Endpoint)
						newReq := req.Clone(context.Background())
						newReq.Header = CloneHeader(req.Header)
						newReq.Host = u.Host
						// Need Fix
						s := strings.Replace(reqBodyStr, "\"replicas\":"+strconv.FormatInt(replicas, 10), "\"replicas\":"+strconv.FormatInt(replicaNum[i], 10), 1)
						reader := bytes.NewReader([]byte(s))
						newReq.ContentLength = int64(reader.Len())
						readCloser := io.NopCloser(reader)
						newReq.Body = readCloser
						requests = append(requests, WaoEndpointRequest{Host: c.Spec.Endpoint, Request: newReq})
					}
				} else {
					reqType = REQ_TYPE_COPY_ALL_DOMAIN
				}
			default:
				reqType = REQ_TYPE_ONLY_MY_DOMAIN
			}
		}
	}

	if reqType == REQ_TYPE_ONLY_MY_DOMAIN || reqType == REQ_TYPE_COPY_ALL_DOMAIN {
		for _, c := range items {
			u, _ := url.Parse(c.Spec.Endpoint)
			newReq := req.Clone(context.Background())
			newReq.Header = CloneHeader(req.Header)
			newReq.Host = u.Host
			reader := bytes.NewReader(reqBody)
			readCloser := io.NopCloser(reader)
			newReq.Body = readCloser
			requests = append(requests, WaoEndpointRequest{Host: c.Spec.Endpoint, Request: newReq})
			if reqType == REQ_TYPE_ONLY_MY_DOMAIN {
				break
			}
		}
	}

	return
}
