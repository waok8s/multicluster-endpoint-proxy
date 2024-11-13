package controller

import (
	"context"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type PodReconciler struct {
	client.Client
}

func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	LABEL_KEY := os.Getenv("LABEL_KEY")
	if _, ok := pod.Labels[LABEL_KEY]; ok {
		var nodeList corev1.NodeList
		requirement, _ := labels.NewRequirement(corev1.LabelTopologyRegion, selection.Exists, []string{})
		err := r.List(ctx, &nodeList, &client.ListOptions{
			Namespace:     "",
			LabelSelector: labels.NewSelector().Add(*requirement),
		})
		if err == nil {
			region := nodeList.Items[0].Labels[corev1.LabelTopologyRegion]
			newPod := pod.DeepCopy()
			newPod.Labels["mep-node"] = pod.Spec.NodeName
			newPod.Labels["mep-region"] = region
			patch := client.MergeFrom(&pod)
			r.Patch(ctx, newPod, patch)
		}
	}

	return ctrl.Result{}, nil
}

func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(ce event.CreateEvent) bool {
				return true
			},
			DeleteFunc: func(de event.DeleteEvent) bool {
				return false
			},
			UpdateFunc: func(ue event.UpdateEvent) bool {
				return true
			},
			GenericFunc: func(ge event.GenericEvent) bool {
				return false
			},
		}).
		Complete(r)
}
