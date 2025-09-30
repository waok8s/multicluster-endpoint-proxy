package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ClusterScoreSpec defines the desired state of ClusterScore
type ClusterScoreSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	Region string `json:"region,omitempty"`

	// +kubebuilder:default=false
	OwnCluster bool `json:"owncluster,omitempty"`
}

type ClusterScoreNodeScore struct {
	Name  string `json:"name"`
	Score int64  `json:"score"`
}

// ClusterScoreStatus defines the observed state of ClusterScore
type ClusterScoreStatus struct {
	Score           int64                   `json:"score,omitempty"`
	Nodes           []ClusterScoreNodeScore `json:"nodes"`
	UpdateTimestamp metav1.Time             `json:"updateTimestamp,omitempty" protobuf:"bytes,8,opt,name=updateTimestamp"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="Score",type=integer,JSONPath=`.status.score`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// ClusterScore is the Schema for the clusterscores API
type ClusterScore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterScoreSpec   `json:"spec,omitempty"`
	Status ClusterScoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterScoreList contains a list of ClusterScore
type ClusterScoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterScore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterScore{}, &ClusterScoreList{})
}
