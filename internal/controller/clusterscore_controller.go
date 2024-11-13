/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	multiclusterendpointproxyv1beta1 "github.com/waok8s/multicluster-endpoint-proxy/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// ClusterScoreReconciler reconciles a ClusterScore object
type ClusterScoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	wao    *Wao
}

//#+kubebuilder:rbac:groups=multicluster-endpoint-proxy.waok8s.github.io,resources=*,verbs=*

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ClusterScore object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *ClusterScoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	var cs multiclusterendpointproxyv1beta1.ClusterScore
	if err := r.Get(ctx, req.NamespacedName, &cs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	//lint:ignore S1002 set true in SetupWithManager
	if cs.Spec.OwnCluster == true {
		score, items := r.wao.ClusterScore(ctx, r.Client, 0, false)
		if score > 0 {
			cs.Status.Score = score
			cs.Status.Nodes = []multiclusterendpointproxyv1beta1.ClusterScoreNodeScore{}
			for _, n := range items {
				cs.Status.Nodes = append(cs.Status.Nodes, multiclusterendpointproxyv1beta1.ClusterScoreNodeScore{Name: n.Name, Score: n.Score})
			}
			if err := r.Status().Patch(ctx, &cs, client.Merge); err != nil {
				return ctrl.Result{Requeue: false}, err
			}
		}
	} else {
		var score int64
		var items []multiclusterendpointproxyv1beta1.ClusterScoreNodeScore
		var region string
		endpoint := cs.Spec.Endpoint
		url := endpoint + "/apis/" + multiclusterendpointproxyv1beta1.GroupVersion.Identifier() + "/namespaces/" + cs.ObjectMeta.Namespace + "/clusterscores"
		scoreReq, _ := http.NewRequest("GET", url, nil)
		scoreReq.SetBasicAuth(os.Getenv("BASIC_AUTH_USERNAME"), os.Getenv("BASIC_AUTH_PASSWORD"))
		httpClient := new(http.Client)
		httpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		scoreResp, err := httpClient.Do(scoreReq)
		if err != nil {
			return ctrl.Result{Requeue: false}, err
		}
		defer scoreResp.Body.Close()
		if scoreResp.StatusCode != http.StatusOK {
			return ctrl.Result{Requeue: false}, nil
		}
		respBody, _ := io.ReadAll(scoreResp.Body)
		var clusterscores multiclusterendpointproxyv1beta1.ClusterScoreList
		json.Unmarshal(respBody, &clusterscores)
		for _, clusterscore := range clusterscores.Items {
			if clusterscore.Spec.OwnCluster {
				region = clusterscore.Spec.Region
				score = clusterscore.Status.Score
				items = clusterscore.Status.Nodes
				break
			}
		}
		if score > 0 && len(region) > 0 {
			if cs.Spec.Region == region {
				cs.Status.Score = score
				cs.Status.Nodes = items
				if err := r.Status().Patch(ctx, &cs, client.Merge); err != nil {
					return ctrl.Result{Requeue: false}, err
				}
			} else {
				newCs := cs.DeepCopy()
				newCs.Spec.Region = region
				newCs.Status.Score = score
				newCs.Status.Nodes = items
				patch := client.MergeFrom(&cs)
				if err := r.Patch(ctx, newCs, patch); err != nil {
					return ctrl.Result{Requeue: false}, err
				}
			}
		}
	}

	return ctrl.Result{}, nil
}

type ticker struct {
	events   chan event.GenericEvent
	interval time.Duration
}

func (t *ticker) Start(ctx context.Context) error {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			t.events <- event.GenericEvent{}
		}
	}
}

func (t *ticker) NeedLeaderElection() bool {
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterScoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.wao = NewWao(mgr)

	FETCH_INTERVAL := os.Getenv("FETCH_INTERVAL")
	fetchInterval, _ := strconv.Atoi(FETCH_INTERVAL)
	if fetchInterval <= 0 {
		fetchInterval = 30
	}

	events := make(chan event.GenericEvent)
	source := source.Channel{
		Source:         events,
		DestBufferSize: 0,
	}
	err := mgr.Add(&ticker{
		events:   events,
		interval: time.Duration(fetchInterval) * time.Second, // c.ProbeInterval,
	})
	if err != nil {
		return err
	}
	handler := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, clientObject client.Object) []reconcile.Request {
		var nodeList corev1.NodeList
		requirement, _ := labels.NewRequirement(corev1.LabelTopologyRegion, selection.Exists, []string{})
		err := r.List(ctx, &nodeList, &client.ListOptions{
			Namespace:     "",
			LabelSelector: labels.NewSelector().Add(*requirement),
		})
		if err == nil {
			region := nodeList.Items[0].Labels[corev1.LabelTopologyRegion]
			if len(region) > 0 {
				MY_POD_NAMESPACE := os.Getenv("MY_POD_NAMESPACE")
				name := strings.ReplaceAll(region, ".", "-")
				config := mgr.GetConfig()
				var cs multiclusterendpointproxyv1beta1.ClusterScore
				err := r.Get(ctx, client.ObjectKey{Namespace: MY_POD_NAMESPACE, Name: name}, &cs)
				if err == nil {
					if cs.Spec.Endpoint != config.Host || cs.Spec.Region != region || !cs.Spec.OwnCluster {
						newCs := cs.DeepCopy()
						newCs.Spec.Endpoint = config.Host
						newCs.Spec.Region = region
						newCs.Spec.OwnCluster = true
						patch := client.MergeFrom(&cs)
						r.Patch(ctx, newCs, patch)
					}
				} else {
					cs := multiclusterendpointproxyv1beta1.ClusterScore{
						ObjectMeta: metav1.ObjectMeta{
							Name:      name,
							Namespace: MY_POD_NAMESPACE,
						},
						Spec: multiclusterendpointproxyv1beta1.ClusterScoreSpec{
							Endpoint:   config.Host,
							Region:     region,
							OwnCluster: true,
						},
					}
					r.Create(ctx, &cs)
				}
			}
		}

		clusterScores := multiclusterendpointproxyv1beta1.ClusterScoreList{}
		mgr.GetCache().List(ctx, &clusterScores)

		var requests []reconcile.Request
		for _, clusterScore := range clusterScores.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterScore.Name,
					Namespace: clusterScore.Namespace,
				},
			})
		}

		return requests
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&multiclusterendpointproxyv1beta1.ClusterScore{}).
		WatchesRawSource(&source, handler).
		Complete(r)
}
