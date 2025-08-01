package cache

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/argoproj/gitops-engine/pkg/utils/kube"
	"github.com/argoproj/gitops-engine/pkg/utils/text"
	"github.com/cespare/xxhash/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	resourcehelper "k8s.io/kubectl/pkg/util/resource"

	"github.com/argoproj/argo-cd/v3/common"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/argo/normalizers"
	"github.com/argoproj/argo-cd/v3/util/resource"
)

func populateNodeInfo(un *unstructured.Unstructured, res *ResourceInfo, customLabels []string) {
	gvk := un.GroupVersionKind()
	revision := resource.GetRevision(un)
	if revision > 0 {
		res.Info = append(res.Info, v1alpha1.InfoItem{Name: "Revision", Value: fmt.Sprintf("Rev:%v", revision)})
	}
	if len(customLabels) > 0 {
		if labels := un.GetLabels(); labels != nil {
			for _, customLabel := range customLabels {
				if value, ok := labels[customLabel]; ok {
					res.Info = append(res.Info, v1alpha1.InfoItem{Name: customLabel, Value: value})
				}
			}
		}
	}

	for k, v := range un.GetAnnotations() {
		if strings.HasPrefix(k, common.AnnotationKeyLinkPrefix) {
			if res.NetworkingInfo == nil {
				res.NetworkingInfo = &v1alpha1.ResourceNetworkingInfo{}
			}
			res.NetworkingInfo.ExternalURLs = append(res.NetworkingInfo.ExternalURLs, v)
		}
	}

	switch gvk.Group {
	case "":
		switch gvk.Kind {
		case kube.PodKind:
			populatePodInfo(un, res)
		case kube.ServiceKind:
			populateServiceInfo(un, res)
		case "Node":
			populateHostNodeInfo(un, res)
		}
	case "extensions", "networking.k8s.io":
		if gvk.Kind == kube.IngressKind {
			populateIngressInfo(un, res)
		}
	case "networking.istio.io":
		switch gvk.Kind {
		case "VirtualService":
			populateIstioVirtualServiceInfo(un, res)
		case "ServiceEntry":
			populateIstioServiceEntryInfo(un, res)
		}
	}
}

func getIngress(un *unstructured.Unstructured) []corev1.LoadBalancerIngress {
	ingress, ok, err := unstructured.NestedSlice(un.Object, "status", "loadBalancer", "ingress")
	if !ok || err != nil {
		return nil
	}
	res := make([]corev1.LoadBalancerIngress, 0)
	for _, item := range ingress {
		if lbIngress, ok := item.(map[string]any); ok {
			if hostname := lbIngress["hostname"]; hostname != nil {
				res = append(res, corev1.LoadBalancerIngress{Hostname: fmt.Sprintf("%s", hostname)})
			} else if ip := lbIngress["ip"]; ip != nil {
				res = append(res, corev1.LoadBalancerIngress{IP: fmt.Sprintf("%s", ip)})
			}
		}
	}
	return res
}

func populateServiceInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	targetLabels, _, _ := unstructured.NestedStringMap(un.Object, "spec", "selector")
	ingress := make([]corev1.LoadBalancerIngress, 0)
	if serviceType, ok, err := unstructured.NestedString(un.Object, "spec", "type"); ok && err == nil && serviceType == string(corev1.ServiceTypeLoadBalancer) {
		ingress = getIngress(un)
	}

	var urls []string
	if res.NetworkingInfo != nil {
		urls = res.NetworkingInfo.ExternalURLs
	}

	res.NetworkingInfo = &v1alpha1.ResourceNetworkingInfo{TargetLabels: targetLabels, Ingress: ingress, ExternalURLs: urls}
}

func getServiceName(backend map[string]any, gvk schema.GroupVersionKind) (string, error) {
	switch gvk.Group {
	case "extensions":
		return fmt.Sprintf("%s", backend["serviceName"]), nil
	case "networking.k8s.io":
		switch gvk.Version {
		case "v1beta1":
			return fmt.Sprintf("%s", backend["serviceName"]), nil
		case "v1":
			if service, ok, err := unstructured.NestedMap(backend, "service"); ok && err == nil {
				return fmt.Sprintf("%s", service["name"]), nil
			}
		}
	}
	return "", errors.New("unable to resolve string")
}

func populateIngressInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	ingress := getIngress(un)
	targetsMap := make(map[v1alpha1.ResourceRef]bool)
	gvk := un.GroupVersionKind()
	if backend, ok, err := unstructured.NestedMap(un.Object, "spec", "backend"); ok && err == nil {
		if serviceName, err := getServiceName(backend, gvk); err == nil {
			targetsMap[v1alpha1.ResourceRef{
				Group:     "",
				Kind:      kube.ServiceKind,
				Namespace: un.GetNamespace(),
				Name:      serviceName,
			}] = true
		}
	}
	urlsSet := make(map[string]bool)
	if rules, ok, err := unstructured.NestedSlice(un.Object, "spec", "rules"); ok && err == nil {
		for i := range rules {
			rule, ok := rules[i].(map[string]any)
			if !ok {
				continue
			}
			host := rule["host"]
			if host == nil || host == "" {
				for i := range ingress {
					host = text.FirstNonEmpty(ingress[i].Hostname, ingress[i].IP)
					if host != "" {
						break
					}
				}
			}
			paths, ok, err := unstructured.NestedSlice(rule, "http", "paths")
			if !ok || err != nil {
				continue
			}
			for i := range paths {
				path, ok := paths[i].(map[string]any)
				if !ok {
					continue
				}

				if backend, ok, err := unstructured.NestedMap(path, "backend"); ok && err == nil {
					if serviceName, err := getServiceName(backend, gvk); err == nil {
						targetsMap[v1alpha1.ResourceRef{
							Group:     "",
							Kind:      kube.ServiceKind,
							Namespace: un.GetNamespace(),
							Name:      serviceName,
						}] = true
					}
				}

				if host == nil || host == "" {
					continue
				}
				stringPort := "http"
				if tls, ok, err := unstructured.NestedSlice(un.Object, "spec", "tls"); ok && err == nil {
					for i := range tls {
						tlsline, ok := tls[i].(map[string]any)
						secretName := tlsline["secretName"]
						if ok && secretName != nil {
							stringPort = "https"
						}
						tlshost := tlsline["host"]
						if tlshost == host {
							stringPort = "https"
							continue
						}
						if hosts := tlsline["hosts"]; hosts != nil {
							tlshosts, ok := tlsline["hosts"].(map[string]any)
							if ok {
								for j := range tlshosts {
									if tlshosts[j] == host {
										stringPort = "https"
									}
								}
							}
						}
					}
				}

				externalURL := fmt.Sprintf("%s://%s", stringPort, host)

				subPath := ""
				if nestedPath, ok, err := unstructured.NestedString(path, "path"); ok && err == nil {
					subPath = strings.TrimSuffix(nestedPath, "*")
				}
				externalURL += subPath
				urlsSet[externalURL] = true
			}
		}
	}
	targets := make([]v1alpha1.ResourceRef, 0)
	for target := range targetsMap {
		targets = append(targets, target)
	}

	var urls []string
	if res.NetworkingInfo != nil {
		urls = res.NetworkingInfo.ExternalURLs
	}
	for url := range urlsSet {
		urls = append(urls, url)
	}
	res.NetworkingInfo = &v1alpha1.ResourceNetworkingInfo{TargetRefs: targets, Ingress: ingress, ExternalURLs: urls}
}

func populateIstioVirtualServiceInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	targetsMap := make(map[v1alpha1.ResourceRef]bool)

	if rules, ok, err := unstructured.NestedSlice(un.Object, "spec", "http"); ok && err == nil {
		for i := range rules {
			rule, ok := rules[i].(map[string]any)
			if !ok {
				continue
			}
			routes, ok, err := unstructured.NestedSlice(rule, "route")
			if !ok || err != nil {
				continue
			}
			for i := range routes {
				route, ok := routes[i].(map[string]any)
				if !ok {
					continue
				}

				if hostName, ok, err := unstructured.NestedString(route, "destination", "host"); ok && err == nil {
					hostSplits := strings.Split(hostName, ".")
					serviceName := hostSplits[0]

					var namespace string
					if len(hostSplits) >= 2 {
						namespace = hostSplits[1]
					} else {
						namespace = un.GetNamespace()
					}

					targetsMap[v1alpha1.ResourceRef{
						Kind:      kube.ServiceKind,
						Name:      serviceName,
						Namespace: namespace,
					}] = true
				}
			}
		}
	}
	targets := make([]v1alpha1.ResourceRef, 0)
	for target := range targetsMap {
		targets = append(targets, target)
	}

	var urls []string
	if res.NetworkingInfo != nil {
		urls = res.NetworkingInfo.ExternalURLs
	}

	res.NetworkingInfo = &v1alpha1.ResourceNetworkingInfo{TargetRefs: targets, ExternalURLs: urls}
}

func populateIstioServiceEntryInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	targetLabels, ok, err := unstructured.NestedStringMap(un.Object, "spec", "workloadSelector", "labels")
	if err != nil {
		return
	}
	if !ok {
		return
	}
	res.NetworkingInfo = &v1alpha1.ResourceNetworkingInfo{
		TargetLabels: targetLabels,
		TargetRefs: []v1alpha1.ResourceRef{{
			Kind: kube.PodKind,
		}},
	}
}

func isPodInitializedConditionTrue(status *corev1.PodStatus) bool {
	for _, condition := range status.Conditions {
		if condition.Type != corev1.PodInitialized {
			continue
		}

		return condition.Status == corev1.ConditionTrue
	}
	return false
}

func isRestartableInitContainer(initContainer *corev1.Container) bool {
	if initContainer == nil {
		return false
	}
	if initContainer.RestartPolicy == nil {
		return false
	}

	return *initContainer.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

func isPodPhaseTerminal(phase corev1.PodPhase) bool {
	return phase == corev1.PodFailed || phase == corev1.PodSucceeded
}

func populatePodInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	pod := corev1.Pod{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(un.Object, &pod)
	if err != nil {
		return
	}
	restarts := 0
	totalContainers := len(pod.Spec.Containers)
	readyContainers := 0

	podPhase := pod.Status.Phase
	reason := string(podPhase)
	if pod.Status.Reason != "" {
		reason = pod.Status.Reason
	}

	imagesSet := make(map[string]bool)
	for _, container := range pod.Spec.InitContainers {
		imagesSet[container.Image] = true
	}
	for _, container := range pod.Spec.Containers {
		imagesSet[container.Image] = true
	}

	res.Images = nil
	for image := range imagesSet {
		res.Images = append(res.Images, image)
	}

	// If the Pod carries {type:PodScheduled, reason:SchedulingGated}, set reason to 'SchedulingGated'.
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Reason == corev1.PodReasonSchedulingGated {
			reason = corev1.PodReasonSchedulingGated
		}
	}

	initContainers := make(map[string]*corev1.Container)
	for i := range pod.Spec.InitContainers {
		initContainers[pod.Spec.InitContainers[i].Name] = &pod.Spec.InitContainers[i]
		if isRestartableInitContainer(&pod.Spec.InitContainers[i]) {
			totalContainers++
		}
	}

	initializing := false
	for i := range pod.Status.InitContainerStatuses {
		container := pod.Status.InitContainerStatuses[i]
		restarts += int(container.RestartCount)
		switch {
		case container.State.Terminated != nil && container.State.Terminated.ExitCode == 0:
			continue
		case isRestartableInitContainer(initContainers[container.Name]) &&
			container.Started != nil && *container.Started:
			if container.Ready {
				readyContainers++
			}
			continue
		case container.State.Terminated != nil:
			// initialization is failed
			if container.State.Terminated.Reason == "" {
				if container.State.Terminated.Signal != 0 {
					reason = fmt.Sprintf("Init:Signal:%d", container.State.Terminated.Signal)
				} else {
					reason = fmt.Sprintf("Init:ExitCode:%d", container.State.Terminated.ExitCode)
				}
			} else {
				reason = "Init:" + container.State.Terminated.Reason
			}
			initializing = true
		case container.State.Waiting != nil && container.State.Waiting.Reason != "" && container.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + container.State.Waiting.Reason
			initializing = true
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(pod.Spec.InitContainers))
			initializing = true
		}
		break
	}
	if !initializing || isPodInitializedConditionTrue(&pod.Status) {
		hasRunning := false
		for i := len(pod.Status.ContainerStatuses) - 1; i >= 0; i-- {
			container := pod.Status.ContainerStatuses[i]

			restarts += int(container.RestartCount)
			switch {
			case container.State.Waiting != nil && container.State.Waiting.Reason != "":
				reason = container.State.Waiting.Reason
			case container.State.Terminated != nil && container.State.Terminated.Reason != "":
				reason = container.State.Terminated.Reason
			case container.State.Terminated != nil && container.State.Terminated.Reason == "":
				if container.State.Terminated.Signal != 0 {
					reason = fmt.Sprintf("Signal:%d", container.State.Terminated.Signal)
				} else {
					reason = fmt.Sprintf("ExitCode:%d", container.State.Terminated.ExitCode)
				}
			case container.Ready && container.State.Running != nil:
				hasRunning = true
				readyContainers++
			}
		}

		// change pod status back to "Running" if there is at least one container still reporting as "Running" status
		if reason == "Completed" && hasRunning {
			reason = "Running"
		}
	}

	// "NodeLost" = https://github.com/kubernetes/kubernetes/blob/cb8ad64243d48d9a3c26b11b2e0945c098457282/pkg/util/node/node.go#L46
	// But depending on the k8s.io/kubernetes package just for a constant
	// is not worth it.
	// See https://github.com/argoproj/argo-cd/issues/5173
	// and https://github.com/kubernetes/kubernetes/issues/90358#issuecomment-617859364
	if pod.DeletionTimestamp != nil && pod.Status.Reason == "NodeLost" {
		reason = "Unknown"
		// If the pod is being deleted and the pod phase is not succeeded or failed, set the reason to "Terminating".
		// See https://github.com/kubernetes/kubectl/issues/1595#issuecomment-2080001023
	} else if pod.DeletionTimestamp != nil && !isPodPhaseTerminal(podPhase) {
		reason = "Terminating"
	}

	if reason != "" {
		res.Info = append(res.Info, v1alpha1.InfoItem{Name: "Status Reason", Value: reason})
	}

	req, _ := resourcehelper.PodRequestsAndLimits(&pod)

	res.PodInfo = &PodInfo{NodeName: pod.Spec.NodeName, ResourceRequests: req, Phase: pod.Status.Phase}

	res.Info = append(res.Info, v1alpha1.InfoItem{Name: "Node", Value: pod.Spec.NodeName})
	res.Info = append(res.Info, v1alpha1.InfoItem{Name: "Containers", Value: fmt.Sprintf("%d/%d", readyContainers, totalContainers)})
	if restarts > 0 {
		res.Info = append(res.Info, v1alpha1.InfoItem{Name: "Restart Count", Value: strconv.Itoa(restarts)})
	}

	// Requests are relevant even for pods in the init phase or pending state (e.g., due to insufficient resources),
	// as they help with diagnosing scheduling and startup issues.
	// requests will be released for terminated pods either with success or failed state termination.
	if !isPodPhaseTerminal(pod.Status.Phase) {
		CPUReq := req[corev1.ResourceCPU]
		MemoryReq := req[corev1.ResourceMemory]

		res.Info = append(res.Info, v1alpha1.InfoItem{Name: common.PodRequestsCPU, Value: strconv.FormatInt(CPUReq.MilliValue(), 10)})
		res.Info = append(res.Info, v1alpha1.InfoItem{Name: common.PodRequestsMEM, Value: strconv.FormatInt(MemoryReq.MilliValue(), 10)})
	}

	var urls []string
	if res.NetworkingInfo != nil {
		urls = res.NetworkingInfo.ExternalURLs
	}

	res.NetworkingInfo = &v1alpha1.ResourceNetworkingInfo{Labels: un.GetLabels(), ExternalURLs: urls}
}

func populateHostNodeInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	node := corev1.Node{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(un.Object, &node)
	if err != nil {
		return
	}
	res.NodeInfo = &NodeInfo{
		Name:       node.Name,
		Capacity:   node.Status.Capacity,
		SystemInfo: node.Status.NodeInfo,
		Labels:     node.Labels,
	}
}

func generateManifestHash(un *unstructured.Unstructured, ignores []v1alpha1.ResourceIgnoreDifferences, overrides map[string]v1alpha1.ResourceOverride, opts normalizers.IgnoreNormalizerOpts) (string, error) {
	normalizer, err := normalizers.NewIgnoreNormalizer(ignores, overrides, opts)
	if err != nil {
		return "", fmt.Errorf("error creating normalizer: %w", err)
	}

	resource := un.DeepCopy()
	err = normalizer.Normalize(resource)
	if err != nil {
		return "", fmt.Errorf("error normalizing resource: %w", err)
	}

	data, err := resource.MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("error marshaling resource: %w", err)
	}
	hash := hash(data)
	return hash, nil
}

func hash(data []byte) string {
	return strconv.FormatUint(xxhash.Sum64(data), 16)
}
