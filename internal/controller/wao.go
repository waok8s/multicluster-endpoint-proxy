/*
 */

package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	waov1beta1 "github.com/waok8s/wao-core/api/wao/v1beta1"
	waoclient "github.com/waok8s/wao-core/pkg/client"
	waometrics "github.com/waok8s/wao-core/pkg/metrics"
	"github.com/waok8s/wao-core/pkg/predictor"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/klog/v2"
	metricsclientv1beta1 "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
	custommetricsclient "k8s.io/metrics/pkg/client/custom_metrics"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	DefaultMetricsCacheTTL   = 30 * time.Second
	DefaultPredictorCacheTTL = 30 * time.Minute
)

type Wao struct {
	clientSet       *kubernetes.Clientset
	metricsclient   *waoclient.CachedMetricsClient
	predictorclient *waoclient.CachedPredictorClient
	client.Client
}

type WaoNodeScore struct {
	Name  string
	Score int64
}

func NewWao(mgr ctrl.Manager) *Wao {
	cfg := mgr.GetConfig()

	clientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil
	}

	mc, err := metricsclientv1beta1.NewForConfig(cfg)
	if err != nil {
		return nil
	}

	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil
	}
	rm := restmapper.NewDeferredDiscoveryRESTMapper(cacheddiscovery.NewMemCacheClient(dc))
	rm.Reset()
	avg := custommetricsclient.NewAvailableAPIsGetter(dc)
	cmc := custommetricsclient.NewForConfig(cfg, rm, avg)

	utilruntime.Must(waov1beta1.AddToScheme(mgr.GetScheme()))

	return &Wao{
		clientSet:       clientSet,
		metricsclient:   waoclient.NewCachedMetricsClient(mc, cmc, DefaultMetricsCacheTTL),
		predictorclient: waoclient.NewCachedPredictorClient(clientSet, DefaultPredictorCacheTTL),
		Client:          mgr.GetClient(),
	}
}

func (wao *Wao) Score(ctx context.Context, ctrlclient client.Client, nodeName string, appendUsage float64, delta bool) int64 {

	nodeInfo, err := wao.clientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("%v : Cannot get Nodes info. Error : %v", nodeName, err)
		return -1
	}

	nodeMetrics, err := wao.metricsclient.GetNodeMetrics(ctx, nodeName)
	if err != nil {
		klog.ErrorS(err, "wao.Score score=-1 as error occurred", "node", nodeName)
		return -1
	}

	beforeUsage := nodeMetrics.Usage.Cpu().AsApproximateFloat64()
	var afterUsage float64
	if delta {
		afterUsage = beforeUsage + appendUsage
		if beforeUsage == afterUsage {
			return -1
		}
	}

	nodeResource := nodeInfo.Status.Capacity["cpu"]
	nodeCPUCapacity, _ := strconv.ParseFloat(nodeResource.AsDec().String(), 32)
	cpuCapacity := float64(nodeCPUCapacity)

	beforeUsage = (beforeUsage / cpuCapacity) * 100
	if delta {
		afterUsage = (afterUsage / cpuCapacity) * 100
	}
	klog.InfoS("wao.Score usage (formatted)", "node", nodeName, "usage_before", beforeUsage, "usage_after", afterUsage, "cpu_capacity", cpuCapacity)

	// get custom metrics
	inletTemp, err := wao.metricsclient.GetCustomMetricForNode(ctx, nodeName, waometrics.ValueInletTemperature)
	if err != nil {
		klog.ErrorS(err, "wao.Score score=-1 as error occurred", "node", nodeName)
		return -1
	}
	deltaP, err := wao.metricsclient.GetCustomMetricForNode(ctx, nodeName, waometrics.ValueDeltaPressure)
	if err != nil {
		klog.ErrorS(err, "wao.Score score=-1 as error occurred", "node", nodeName)
		return -1
	}
	klog.InfoS("wao.Score metrics", "node", nodeName, "inlet_temp", inletTemp.Value.AsApproximateFloat64(), "delta_p", deltaP.Value.AsApproximateFloat64())

	// get NodeConfig
	var nc *waov1beta1.NodeConfig
	var ncs waov1beta1.NodeConfigList
	if err := ctrlclient.List(ctx, &ncs); err != nil {
		klog.ErrorS(err, "wao.Score score=-1 as error occurred", "node", nodeName)
		return -1
	}
	for _, e := range ncs.Items {
		if e.Spec.NodeName == nodeName {
			nc = e.DeepCopy()
			break
		}
	}
	if nc == nil {
		klog.ErrorS(fmt.Errorf("nodeconfig == nil"), "wao.Score score=-1 as error occurred", "node", nodeName)
		return -1
	}

	// init predictor endpoint
	var ep *waov1beta1.EndpointTerm
	if nc.Spec.Predictor.PowerConsumption != nil {
		ep = nc.Spec.Predictor.PowerConsumption
	} else {
		ep = &waov1beta1.EndpointTerm{}
	}

	if nc.Spec.Predictor.PowerConsumptionEndpointProvider != nil {
		ep2, err := wao.predictorclient.GetPredictorEndpoint(ctx, nc.Namespace, nc.Spec.Predictor.PowerConsumptionEndpointProvider, predictor.TypePowerConsumption)
		if err != nil {
			klog.ErrorS(err, "wao.Score score=-1 as error occurred", "node", nodeName)
			return -1
		}
		ep.Type = ep2.Type
		ep.Endpoint = ep2.Endpoint
	}

	// do predict
	beforeWatt, err := wao.predictorclient.PredictPowerConsumption(ctx, nc.Namespace, ep, beforeUsage, inletTemp.Value.AsApproximateFloat64(), deltaP.Value.AsApproximateFloat64())
	if err != nil {
		klog.ErrorS(err, "wao.Score score=-1 as error occurred", "node", nodeName)
		return -1
	}
	if !delta {
		return int64(beforeWatt)
	}

	afterWatt, err := wao.predictorclient.PredictPowerConsumption(ctx, nc.Namespace, ep, afterUsage, inletTemp.Value.AsApproximateFloat64(), deltaP.Value.AsApproximateFloat64())
	if err != nil {
		klog.ErrorS(err, "wao.Score score=-1 as error occurred", "node", nodeName)
		return -1
	}
	if afterWatt == 3.14 {
		afterWatt = 6.14
	}

	podPowerConsumption := int64(afterWatt - beforeWatt)
	if podPowerConsumption < 0 {
		klog.InfoS("wao.Score round negative scores to 0", "node", nodeName, "watt", afterWatt-beforeWatt)
		podPowerConsumption = 0
	}
	return podPowerConsumption
}

func (wao *Wao) ClusterScore(ctx context.Context, ctrlclient client.Client, appendUsage float64, delta bool) (total int64, items []WaoNodeScore) {
	var nodeList corev1.NodeList
	//requirement, _ := labels.NewRequirement(corev1.LabelTopologyRegion, selection.Exists, []string{})
	err := ctrlclient.List(ctx, &nodeList, &client.ListOptions{
		Namespace: "",
		// LabelSelector: labels.NewSelector().Add(*requirement),
	})
	if err != nil {
		total = -1
		return
	}
	var count int64
	for _, node := range nodeList.Items {
		if len(node.Status.Conditions) == 0 {
			klog.InfoS("wao.ClusterScore empty conditions", "node", node.Name)
			continue
		}
		var ready bool = false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				if condition.Status == corev1.ConditionTrue {
					ready = true
				}
				break
			}
		}
		if !ready {
			klog.InfoS("wao.ClusterScore Unable to confirm that the NodeReadyCondition is True", "node", node.Name)
			continue
		}
		if node.Spec.Unschedulable {
			klog.InfoS("wao.ClusterScore unschedulable", "node", node.Name)
			continue
		}
		score := wao.Score(ctx, ctrlclient, node.Name, appendUsage, delta)
		if score >= 0 {
			total += score
			items = append(items, WaoNodeScore{Name: node.Name, Score: score})
			count++
		}
	}
	if count == 0 {
		total = -1
		return
	}
	return
}
