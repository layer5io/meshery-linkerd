package expose

import (
	"fmt"
	"strconv"

	"github.com/layer5io/meshkit/logger"
	appsv1 "k8s.io/api/apps/v1"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	appsv1beta2 "k8s.io/api/apps/v1beta2"
	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// Config is a the struct for Expose configuration
type Config struct {
	Port           string
	Type           string
	LoadBalancerIP string
	ClusterIP      string
	TargetPort     string
	// Namespace will get overridden as
	// per the namespace of the resource
	Namespace       string
	SessionAffinity string
	Name            string
	Logger          logger.Handler
}

type serviceConfig struct {
	selectorsMap map[string]string
	labelsMap    map[string]string
	protocolsMap map[string]string
	portsSlice   []string
	Config
}

// Expose exposes the given kubernetes resource
func Expose(clientSet *kubernetes.Clientset, restConfig rest.Config, ec Config, resources []string) error {
	// continueOnError not only controls if the traversal should continue even after errors
	continueOnError := true

	tr := Traverser{
		Client:    clientSet,
		Resources: resources,
		Logger:    ec.Logger,
	}
	err := tr.Visit(func(info runtime.Object, ns string, err error) error {
		// Override the namespace
		// as per the namespace
		// of the resource
		ec.Namespace = ns
		if err != nil {
			return nil
		}

		// Check if the resource can be exposed or not
		gk := info.GetObjectKind().GroupVersionKind().GroupKind()
		if err := canBeExposed(gk); err != nil {
			return ErrResourceCannotBeExposed(err, gk.Kind)
		}

		if len(ec.Name) > validation.DNS1035LabelMaxLength {
			ec.Name = ec.Name[:validation.DNS1035LabelMaxLength]
		}

		// Map for selectors of the current object
		selectorsMap, err := mapBasedSelectorForObject(info)
		if err != nil {
			return ErrSelectorBasedMap(err)
		}

		isHeadlessService := ec.ClusterIP == "None"

		// protocolsMap stores the protocols for the current object
		protocolsMap, err := protocolsForObject(info)
		if err != nil {
			return ErrProtocolBasedMap(err)
		}

		// protocolsMap stores the protocols for the current object
		labelsMap, err := meta.NewAccessor().Labels(info)
		if err != nil {
			return ErrLableBasedMap(err)
		}

		ports, err := portsForObject(info)
		if err != nil {
			return ErrPortParsing(err)
		}
		if len(ports) == 0 && !isHeadlessService {
			return ErrPortParsing(fmt.Errorf("no ports found for the non headless resource"))
		}

		service, err := generateService(serviceConfig{
			selectorsMap: selectorsMap,
			protocolsMap: protocolsMap,
			labelsMap:    labelsMap,
			portsSlice:   ports,
			Config:       ec,
		})
		if err != nil {
			return ErrGenerateService(err)
		}
		ec.Logger.Info(fmt.Sprintf("Generated service object %s in namespace %s", service.Name, service.Namespace))
		helper, err := constructObject(clientSet, restConfig, service)
		if err != nil {
			return ErrConstructingRestHelper(err)
		}

		_, err = helper.Create(ec.Namespace, false, service)
		ec.Logger.Info("Service deployed")
		if err != nil {
			return ErrCreatingService(err)
		}

		return nil
	}, continueOnError)

	// This err is already of generated
	// from ErrTraverser
	return err
}

func generateService(serviceConfig serviceConfig) (*corev1.Service, error) {
	ports := []corev1.ServicePort{}
	for i, port := range serviceConfig.portsSlice {
		// We can expect the port to be a valid UNIX port and hence
		// should not cause integer overflow. Hence,
		// #nosec
		portInt, err := strconv.Atoi(port)
		if err != nil {
			return nil, err
		}
		portName := ""

		if len(serviceConfig.portsSlice) > 1 {
			portName = fmt.Sprintf("port-%d", i+1)
		}

		protocol := "TCP" // Default protocol is "TCP"
		if exposeProtocol, ok := serviceConfig.protocolsMap[port]; ok {
			protocol = exposeProtocol
		}

		ports = append(ports, corev1.ServicePort{
			Name:     portName,
			Port:     int32(portInt),
			Protocol: corev1.Protocol(protocol),
		})
	}

	service := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceConfig.Name,
			Labels:    serviceConfig.labelsMap,
			Namespace: serviceConfig.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: serviceConfig.selectorsMap,
			Ports:    ports,
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
	}

	// Setup target ports
	for i := range service.Spec.Ports {
		port := service.Spec.Ports[i].Port
		service.Spec.Ports[i].TargetPort = intstr.FromInt(int(port))
	}

	// Setup service type
	if len(serviceConfig.Type) != 0 {
		service.Spec.Type = corev1.ServiceType(serviceConfig.Type)
	}

	// Setup load balancer ip if the type is load balancer
	if service.Spec.Type == corev1.ServiceTypeLoadBalancer {
		service.Spec.LoadBalancerIP = serviceConfig.LoadBalancerIP
	}

	// Setup session affinity
	if len(serviceConfig.SessionAffinity) != 0 {
		switch corev1.ServiceAffinity(serviceConfig.SessionAffinity) {
		case corev1.ServiceAffinityNone:
			service.Spec.SessionAffinity = corev1.ServiceAffinityNone
		case corev1.ServiceAffinityClientIP:
			service.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
		default:
			return nil, fmt.Errorf("unknown session affinity: %s", serviceConfig.SessionAffinity)
		}
	}

	// Setup cluster IP
	if len(serviceConfig.ClusterIP) != 0 {
		if serviceConfig.ClusterIP == "None" {
			service.Spec.ClusterIP = corev1.ClusterIPNone
		} else {
			service.Spec.ClusterIP = serviceConfig.ClusterIP
		}
	}

	return &service, nil
}

// canBeExposed checks whether the kind of resources could be exposed
func canBeExposed(kind schema.GroupKind) error {
	switch kind {
	case
		corev1.SchemeGroupVersion.WithKind("ReplicationController").GroupKind(),
		corev1.SchemeGroupVersion.WithKind("Service").GroupKind(),
		corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(),
		appsv1.SchemeGroupVersion.WithKind("Deployment").GroupKind(),
		appsv1.SchemeGroupVersion.WithKind("ReplicaSet").GroupKind(),
		extensionsv1beta1.SchemeGroupVersion.WithKind("Deployment").GroupKind(),
		extensionsv1beta1.SchemeGroupVersion.WithKind("ReplicaSet").GroupKind():
	default:
		return fmt.Errorf("cannot expose a %s", kind)
	}
	return nil
}

// mapBasedSelectorForObject returns the map-based selector associated with the provided object. If a
// new set-based selector is provided, an error is returned if the selector cannot be converted to a
// map-based selector
func mapBasedSelectorForObject(object runtime.Object) (map[string]string, error) {
	switch t := object.(type) {
	case *corev1.ReplicationController:
		return t.Spec.Selector, nil

	case *corev1.Pod:
		if len(t.Labels) == 0 {
			return map[string]string{}, fmt.Errorf("the pod has no labels and cannot be exposed")
		}
		return t.Labels, nil

	case *corev1.Service:
		if t.Spec.Selector == nil {
			return map[string]string{}, fmt.Errorf("the service has no pod selector set")
		}
		return t.Spec.Selector, nil

	case *extensionsv1beta1.Deployment:
		// "extensions" deployments use pod template labels if selector is not set.
		var labels map[string]string
		if t.Spec.Selector != nil {
			if len(t.Spec.Selector.MatchExpressions) > 0 {
				return map[string]string{}, fmt.Errorf("couldn't convert expressions - \"%+v\" to map-based selector format", t.Spec.Selector.MatchExpressions)
			}
			labels = t.Spec.Selector.MatchLabels
		} else {
			labels = t.Spec.Template.Labels
		}
		if len(labels) == 0 {
			return map[string]string{}, fmt.Errorf("the deployment has no labels or selectors and cannot be exposed")
		}
		return labels, nil

	case *appsv1.Deployment:
		// "apps" deployments must have the selector set.
		if t.Spec.Selector == nil || len(t.Spec.Selector.MatchLabels) == 0 {
			return map[string]string{}, fmt.Errorf("invalid deployment: no selectors, therefore cannot be exposed")
		}
		if len(t.Spec.Selector.MatchExpressions) > 0 {
			return map[string]string{}, fmt.Errorf("couldn't convert expressions - \"%+v\" to map-based selector format", t.Spec.Selector.MatchExpressions)
		}
		return t.Spec.Selector.MatchLabels, nil

	case *appsv1beta2.Deployment:
		// "apps" deployments must have the selector set.
		if t.Spec.Selector == nil || len(t.Spec.Selector.MatchLabels) == 0 {
			return map[string]string{}, fmt.Errorf("invalid deployment: no selectors, therefore cannot be exposed")
		}
		if len(t.Spec.Selector.MatchExpressions) > 0 {
			return map[string]string{}, fmt.Errorf("couldn't convert expressions - \"%+v\" to map-based selector format", t.Spec.Selector.MatchExpressions)
		}
		return t.Spec.Selector.MatchLabels, nil

	case *appsv1beta1.Deployment:
		// "apps" deployments must have the selector set.
		if t.Spec.Selector == nil || len(t.Spec.Selector.MatchLabels) == 0 {
			return map[string]string{}, fmt.Errorf("invalid deployment: no selectors, therefore cannot be exposed")
		}
		if len(t.Spec.Selector.MatchExpressions) > 0 {
			return map[string]string{}, fmt.Errorf("couldn't convert expressions - \"%+v\" to map-based selector format", t.Spec.Selector.MatchExpressions)
		}
		return t.Spec.Selector.MatchLabels, nil

	case *extensionsv1beta1.ReplicaSet:
		// "extensions" replicasets use pod template labels if selector is not set.
		var labels map[string]string
		if t.Spec.Selector != nil {
			if len(t.Spec.Selector.MatchExpressions) > 0 {
				return map[string]string{}, fmt.Errorf("couldn't convert expressions - \"%+v\" to map-based selector format", t.Spec.Selector.MatchExpressions)
			}
			labels = t.Spec.Selector.MatchLabels
		} else {
			labels = t.Spec.Template.Labels
		}
		if len(labels) == 0 {
			return map[string]string{}, fmt.Errorf("the replica set has no labels or selectors and cannot be exposed")
		}
		return labels, nil

	case *appsv1.ReplicaSet:
		// "apps" replicasets must have the selector set.
		if t.Spec.Selector == nil || len(t.Spec.Selector.MatchLabels) == 0 {
			return map[string]string{}, fmt.Errorf("invalid replicaset: no selectors, therefore cannot be exposed")
		}
		if len(t.Spec.Selector.MatchExpressions) > 0 {
			return map[string]string{}, fmt.Errorf("couldn't convert expressions - \"%+v\" to map-based selector format", t.Spec.Selector.MatchExpressions)
		}
		return t.Spec.Selector.MatchLabels, nil

	case *appsv1beta2.ReplicaSet:
		// "apps" replicasets must have the selector set.
		if t.Spec.Selector == nil || len(t.Spec.Selector.MatchLabels) == 0 {
			return map[string]string{}, fmt.Errorf("invalid replicaset: no selectors, therefore cannot be exposed")
		}
		if len(t.Spec.Selector.MatchExpressions) > 0 {
			return map[string]string{}, fmt.Errorf("couldn't convert expressions - \"%+v\" to map-based selector format", t.Spec.Selector.MatchExpressions)
		}
		return t.Spec.Selector.MatchLabels, nil

	default:
		return map[string]string{}, fmt.Errorf("cannot extract pod selector from %T", object)
	}
}

func protocolsForObject(object runtime.Object) (map[string]string, error) {
	switch t := object.(type) {
	case *corev1.ReplicationController:
		return getProtocols(t.Spec.Template.Spec), nil

	case *corev1.Pod:
		return getProtocols(t.Spec), nil

	case *corev1.Service:
		return getServiceProtocols(t.Spec), nil

	case *extensionsv1beta1.Deployment:
		return getProtocols(t.Spec.Template.Spec), nil
	case *appsv1.Deployment:
		return getProtocols(t.Spec.Template.Spec), nil
	case *appsv1beta2.Deployment:
		return getProtocols(t.Spec.Template.Spec), nil
	case *appsv1beta1.Deployment:
		return getProtocols(t.Spec.Template.Spec), nil

	case *extensionsv1beta1.ReplicaSet:
		return getProtocols(t.Spec.Template.Spec), nil
	case *appsv1.ReplicaSet:
		return getProtocols(t.Spec.Template.Spec), nil
	case *appsv1beta2.ReplicaSet:
		return getProtocols(t.Spec.Template.Spec), nil

	default:
		return nil, fmt.Errorf("cannot extract protocols from %T", object)
	}
}

func getProtocols(spec corev1.PodSpec) map[string]string {
	result := make(map[string]string)
	for _, container := range spec.Containers {
		for _, port := range container.Ports {
			// Empty protocol must be defaulted (TCP)
			if len(port.Protocol) == 0 {
				port.Protocol = corev1.ProtocolTCP
			}
			result[strconv.Itoa(int(port.ContainerPort))] = string(port.Protocol)
		}
	}
	return result
}

// Extracts the protocols exposed by a service from the given service spec.
func getServiceProtocols(spec corev1.ServiceSpec) map[string]string {
	result := make(map[string]string)
	for _, servicePort := range spec.Ports {
		// Empty protocol must be defaulted (TCP)
		if len(servicePort.Protocol) == 0 {
			servicePort.Protocol = corev1.ProtocolTCP
		}
		result[strconv.Itoa(int(servicePort.Port))] = string(servicePort.Protocol)
	}
	return result
}

func portsForObject(object runtime.Object) ([]string, error) {
	switch t := object.(type) {
	case *corev1.ReplicationController:
		return getPorts(t.Spec.Template.Spec), nil

	case *corev1.Pod:
		return getPorts(t.Spec), nil

	case *corev1.Service:
		return getServicePorts(t.Spec), nil

	case *extensionsv1beta1.Deployment:
		return getPorts(t.Spec.Template.Spec), nil
	case *appsv1.Deployment:
		return getPorts(t.Spec.Template.Spec), nil
	case *appsv1beta2.Deployment:
		return getPorts(t.Spec.Template.Spec), nil
	case *appsv1beta1.Deployment:
		return getPorts(t.Spec.Template.Spec), nil

	case *extensionsv1beta1.ReplicaSet:
		return getPorts(t.Spec.Template.Spec), nil
	case *appsv1.ReplicaSet:
		return getPorts(t.Spec.Template.Spec), nil
	case *appsv1beta2.ReplicaSet:
		return getPorts(t.Spec.Template.Spec), nil
	default:
		return nil, fmt.Errorf("cannot extract ports from %T", object)
	}
}

func getPorts(spec corev1.PodSpec) []string {
	result := []string{}
	for _, container := range spec.Containers {
		for _, port := range container.Ports {
			result = append(result, strconv.Itoa(int(port.ContainerPort)))
		}
	}
	return result
}

func getServicePorts(spec corev1.ServiceSpec) []string {
	result := []string{}
	for _, servicePort := range spec.Ports {
		result = append(result, strconv.Itoa(int(servicePort.Port)))
	}
	return result
}

func constructObject(kubeClientset kubernetes.Interface, restConfig rest.Config, obj runtime.Object) (*resource.Helper, error) {
	// Create a REST mapper that tracks information about the available resources in the cluster.
	groupResources, err := restmapper.GetAPIGroupResources(kubeClientset.Discovery())
	if err != nil {
		return nil, err
	}
	rm := restmapper.NewDiscoveryRESTMapper(groupResources)

	// Get some metadata needed to make the REST request.
	gvk := obj.GetObjectKind().GroupVersionKind()
	gk := schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}
	mapping, err := rm.RESTMapping(gk, gvk.Version)
	if err != nil {
		return nil, err
	}

	// Create a client specifically for creating the object.
	restClient, err := newRestClient(restConfig, mapping.GroupVersionKind.GroupVersion())
	if err != nil {
		return nil, err
	}

	// Use the REST helper to create the object in the "default" namespace.
	return resource.NewHelper(restClient, mapping), nil
}

func newRestClient(restConfig rest.Config, gv schema.GroupVersion) (rest.Interface, error) {
	restConfig.ContentConfig = resource.UnstructuredPlusDefaultContentConfig()
	restConfig.GroupVersion = &gv
	if len(gv.Group) == 0 {
		restConfig.APIPath = "/api"
	} else {
		restConfig.APIPath = "/apis"
	}

	return rest.RESTClientFor(&restConfig)
}
