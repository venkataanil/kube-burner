// Copyright 2024 The Kube-burner Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package measurements

import (
	"context"
	"sync"
	"time"

	"github.com/cloud-bulldozer/go-commons/v2/indexers"
	"github.com/kube-burner/kube-burner/pkg/config"
	"github.com/kube-burner/kube-burner/pkg/measurements/metrics"
	"github.com/kube-burner/kube-burner/pkg/measurements/types"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	nodeLatencyMeasurement          = "nodeLatencyMeasurement"
	nodeLatencyQuantilesMeasurement = "nodeLatencyQuantilesMeasurement"
)

var (
	supportedNodeConditions = map[string]struct{}{
		string(corev1.NodeMemoryPressure): {},
		string(corev1.NodeDiskPressure):   {},
		string(corev1.NodePIDPressure):    {},
		string(corev1.NodeReady):          {},
	}
)

type NodeMetric struct {
	Timestamp                 time.Time         `json:"timestamp"`
	NodeMemoryPressure        time.Time         `json:"-"`
	NodeMemoryPressureLatency int               `json:"nodeMemoryPressureLatency"`
	NodeDiskPressure          time.Time         `json:"-"`
	NodeDiskPressureLatency   int               `json:"nodeDiskPressureLatency"`
	NodePIDPressure           time.Time         `json:"-"`
	NodePIDPressureLatency    int               `json:"nodePIDPressureLatency"`
	NodeReady                 time.Time         `json:"-"`
	NodeReadyLatency          int               `json:"nodeReadyLatency"`
	MetricName                string            `json:"metricName"`
	UUID                      string            `json:"uuid"`
	JobName                   string            `json:"jobName,omitempty"`
	Name                      string            `json:"nodeName"`
	Labels                    map[string]string `json:"labels"`
	Metadata                  interface{}       `json:"metadata,omitempty"`
}

type nodeLatency struct {
	BaseMeasurement

	watcher          *metrics.Watcher
	metrics          sync.Map
	latencyQuantiles []interface{}
	normLatencies    []interface{}
}

type nodeLatencyMeasurementFactory struct {
	BaseMeasurementFactory
}

func newNodeLatencyMeasurementFactory(configSpec config.Spec, measurement types.Measurement, metadata map[string]interface{}) (MeasurementFactory, error) {
	if err := VerifyMeasurementConfig(measurement, supportedNodeConditions); err != nil {
		return nil, err
	}
	return nodeLatencyMeasurementFactory{
		BaseMeasurementFactory: NewBaseMeasurementFactory(configSpec, measurement, metadata),
	}, nil
}

func (nlmf nodeLatencyMeasurementFactory) NewMeasurement(jobConfig *config.Job, clientSet kubernetes.Interface, restConfig *rest.Config) Measurement {
	return &nodeLatency{
		BaseMeasurement: nlmf.NewBaseLatency(jobConfig, clientSet, restConfig),
	}
}

func (n *nodeLatency) handleCreateNode(obj interface{}) {
	node := obj.(*corev1.Node)
	labels := node.Labels
	n.metrics.LoadOrStore(string(node.UID), NodeMetric{
		Timestamp:  node.CreationTimestamp.Time.UTC(),
		Name:       node.Name,
		MetricName: nodeLatencyMeasurement,
		UUID:       n.Uuid,
		JobName:    n.JobConfig.Name,
		Labels:     labels,
		Metadata:   n.Metadata,
	})
}

func (n *nodeLatency) handleUpdateNode(obj interface{}) {
	node := obj.(*corev1.Node)
	if value, exists := n.metrics.Load(string(node.UID)); exists {
		nm := value.(NodeMetric)
		for _, c := range node.Status.Conditions {
			switch c.Type {
			case corev1.NodeMemoryPressure:
				if c.Status == corev1.ConditionFalse {
					nm.NodeMemoryPressure = c.LastTransitionTime.Time.UTC()
				}
			case corev1.NodeDiskPressure:
				if c.Status == corev1.ConditionFalse {
					nm.NodeDiskPressure = c.LastTransitionTime.Time.UTC()
				}
			case corev1.NodePIDPressure:
				if c.Status == corev1.ConditionFalse {
					nm.NodePIDPressure = c.LastTransitionTime.Time.UTC()
				}
			case corev1.NodeReady:
				if c.Status == corev1.ConditionTrue {
					log.Debugf("Node %s is ready", node.Name)
					nm.NodeReady = c.LastTransitionTime.Time.UTC()
				}
			}
		}
		n.metrics.Store(string(node.UID), nm)
	}
}

func (n *nodeLatency) Start(measurementWg *sync.WaitGroup) error {
	n.latencyQuantiles, n.normLatencies = nil, nil
	defer measurementWg.Done()
	var wg sync.WaitGroup
	wg.Add(1)
	n.Collect(&wg)
	wg.Wait()
	log.Infof("Creating Node latency watcher for %s", n.JobConfig.Name)
	n.watcher = metrics.NewWatcher(
		n.ClientSet.CoreV1().RESTClient().(*rest.RESTClient),
		"nodeWatcher",
		"nodes",
		corev1.NamespaceAll,
		func(options *metav1.ListOptions) {
		},
		nil,
	)
	n.watcher.Informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: n.handleCreateNode,
		UpdateFunc: func(oldObj, newObj interface{}) {
			n.handleUpdateNode(newObj)
		},
	})
	if err := n.watcher.StartAndCacheSync(); err != nil {
		log.Errorf("Node Latency measurement error: %s", err)
	}
	return nil
}

func (n *nodeLatency) Collect(measurementWg *sync.WaitGroup) {
	defer measurementWg.Done()
	var nodes []corev1.Node
	nodeList, err := n.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Errorf("error listing nodes: %v", err)
	}
	nodes = append(nodes, nodeList.Items...)

	n.metrics = sync.Map{}
	for _, node := range nodes {
		var nodeMemoryPressure, nodeDiskPressure, nodePIDPressure, nodeReady time.Time
		for _, c := range node.Status.Conditions {
			switch c.Type {
			case corev1.NodeMemoryPressure:
				nodeMemoryPressure = c.LastTransitionTime.Time.UTC()
			case corev1.NodeDiskPressure:
				nodeDiskPressure = c.LastTransitionTime.Time.UTC()
			case corev1.NodePIDPressure:
				nodePIDPressure = c.LastTransitionTime.Time.UTC()
			case corev1.NodeReady:
				nodeReady = c.LastTransitionTime.Time.UTC()
			}
		}
		n.metrics.Store(string(node.UID), NodeMetric{
			Timestamp:          node.CreationTimestamp.Time.UTC(),
			Name:               node.Name,
			MetricName:         nodeLatencyMeasurement,
			UUID:               n.Uuid,
			NodeMemoryPressure: nodeMemoryPressure,
			NodeDiskPressure:   nodeDiskPressure,
			NodePIDPressure:    nodePIDPressure,
			NodeReady:          nodeReady,
			JobName:            n.JobConfig.Name,
			Labels:             node.Labels,
		})
	}
}

func (n *nodeLatency) Stop() error {
	var err error
	defer func() {
		if n.watcher != nil {
			n.watcher.StopWatcher()
		}
	}()
	n.normalizeLatencies()
	n.CalculateQuantiles()
	if len(n.Config.LatencyThresholds) > 0 {
		err = metrics.CheckThreshold(n.Config.LatencyThresholds, n.latencyQuantiles)
	}
	if n.JobConfig.Name != "workers-scale" {
		for _, q := range n.latencyQuantiles {
			nq := q.(metrics.LatencyQuantiles)
			log.Infof("%s: %s 50th: %v 99th: %v max: %v avg: %v", n.JobConfig.Name, nq.QuantileName, nq.P50, nq.P99, nq.Max, nq.Avg)
		}
	}
	return err
}

func (n *nodeLatency) Index(jobName string, indexerList map[string]indexers.Indexer) {
	metricMap := map[string][]interface{}{
		nodeLatencyMeasurement:          n.normLatencies,
		nodeLatencyQuantilesMeasurement: n.latencyQuantiles,
	}
	IndexLatencyMeasurement(n.Config, jobName, metricMap, indexerList)
}

func (n *nodeLatency) GetMetrics() *sync.Map {
	return &n.metrics
}

func (n *nodeLatency) normalizeLatencies() {
	n.metrics.Range(func(key, value interface{}) bool {
		m := value.(NodeMetric)
		// If a node does not reach the Ready state, we skip that node
		if m.NodeReady.IsZero() {
			log.Tracef("Node %v latency ignored as it did not reach Ready state", m.Name)
			return true
		}
		earliest := m.Timestamp
		if m.NodeMemoryPressure.Before(earliest) {
			earliest = m.NodeMemoryPressure
		}
		if m.NodeDiskPressure.Before(earliest) {
			earliest = m.NodeDiskPressure
		}
		if m.NodePIDPressure.Before(earliest) {
			earliest = m.NodePIDPressure
		}
		m.NodeMemoryPressureLatency = int(m.NodeMemoryPressure.Sub(earliest).Milliseconds())
		m.NodeDiskPressureLatency = int(m.NodeDiskPressure.Sub(earliest).Milliseconds())
		m.NodePIDPressureLatency = int(m.NodePIDPressure.Sub(earliest).Milliseconds())
		m.NodeReadyLatency = int(m.NodeReady.Sub(earliest).Milliseconds())
		n.normLatencies = append(n.normLatencies, m)
		return true
	})
}

func (n *nodeLatency) CalculateQuantiles() {
	getLatency := func(normLatency interface{}) map[string]float64 {
		nodeMetric := normLatency.(NodeMetric)
		return map[string]float64{
			string(corev1.NodeMemoryPressure): float64(nodeMetric.NodeMemoryPressureLatency),
			string(corev1.NodeDiskPressure):   float64(nodeMetric.NodeDiskPressureLatency),
			string(corev1.NodePIDPressure):    float64(nodeMetric.NodePIDPressureLatency),
			string(corev1.NodeReady):          float64(nodeMetric.NodeReadyLatency),
		}
	}
	n.latencyQuantiles = CalculateQuantiles(n.Uuid, n.JobConfig.Name, n.Metadata, n.normLatencies, getLatency, nodeLatencyQuantilesMeasurement)
}
