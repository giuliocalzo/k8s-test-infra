// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package labels

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// NewNodePatcher builds a NodePatcher from the pod's ServiceAccount. It fails
// when there is no in-cluster config — running outside Kubernetes, say — and
// callers are expected to degrade to a warning rather than treat that as fatal.
func NewNodePatcher() (NodePatcher, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := clientset.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return nodePatcher{nodes: cs.CoreV1().Nodes()}, nil
}

type nodePatcher struct {
	nodes corev1client.NodeInterface
}

// Patch sends a JSON merge patch, which needs only the `patch` verb the chart's
// ClusterRole already grants — no read-modify-write and no `update`.
func (p nodePatcher) Patch(ctx context.Context, nodeName string, patch []byte) error {
	_, err := p.nodes.Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
