package ingresscontroller

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/managed-cluster-validating-webhooks/pkg/webhooks/utils"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	admissionctl "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	WebhookName                     string = "ingresscontroller-validation"
	docString                       string = `Managed OpenShift Customer may create IngressControllers without necessary taints. This can cause those workloads to be provisioned on master nodes.`
	legacyIngressSupportFeatureFlag        = "ext-managed.openshift.io/legacy-ingress-support"
)

var (
	log   = logf.Log.WithName(WebhookName)
	scope = admissionregv1.NamespacedScope
	rules = []admissionregv1.RuleWithOperations{
		{
			Operations: []admissionregv1.OperationType{admissionregv1.Create, admissionregv1.Update},
			Rule: admissionregv1.Rule{
				APIGroups:   []string{"operator.openshift.io"},
				APIVersions: []string{"*"},
				Resources:   []string{"ingresscontroller", "ingresscontrollers"},
				Scope:       &scope,
			},
		},
	}
	allowedUsers = []string{
		"backplane-cluster-admin",
	}
)

type IngressControllerWebhook struct {
	s runtime.Scheme
}

// ObjectSelector implements Webhook interface
func (wh *IngressControllerWebhook) ObjectSelector() *metav1.LabelSelector { return nil }

func (wh *IngressControllerWebhook) Doc() string {
	return fmt.Sprintf(docString)
}

// TimeoutSeconds implements Webhook interface
func (wh *IngressControllerWebhook) TimeoutSeconds() int32 { return 1 }

// MatchPolicy implements Webhook interface
func (wh *IngressControllerWebhook) MatchPolicy() admissionregv1.MatchPolicyType {
	return admissionregv1.Equivalent
}

// Name implements Webhook interface
func (wh *IngressControllerWebhook) Name() string { return WebhookName }

// FailurePolicy implements Webhook interface and defines how unrecognized errors and timeout errors from the admission webhook are handled. Allowed values are Ignore or Fail.
// Ignore means that an error calling the webhook is ignored and the API request is allowed to continue.
// It's important to leave the FailurePolicy set to Ignore because otherwise the pod will fail to be created as the API request will be rejected.
func (wh *IngressControllerWebhook) FailurePolicy() admissionregv1.FailurePolicyType {
	return admissionregv1.Ignore
}

// Rules implements Webhook interface
func (wh *IngressControllerWebhook) Rules() []admissionregv1.RuleWithOperations { return rules }

// GetURI implements Webhook interface
func (wh *IngressControllerWebhook) GetURI() string { return "/" + WebhookName }

// SideEffects implements Webhook interface
func (wh *IngressControllerWebhook) SideEffects() admissionregv1.SideEffectClass {
	return admissionregv1.SideEffectClassNone
}

// Validate implements Webhook interface
func (wh *IngressControllerWebhook) Validate(req admissionctl.Request) bool {
	valid := true
	valid = valid && (req.UserInfo.Username != "")
	valid = valid && (req.Kind.Kind == "IngressController")

	return valid
}

func (wh *IngressControllerWebhook) renderIngressController(req admissionctl.Request) (*operatorv1.IngressController, error) {
	decoder, err := admissionctl.NewDecoder(&wh.s)
	if err != nil {
		return nil, err
	}
	ic := &operatorv1.IngressController{}
	err = decoder.DecodeRaw(req.Object, ic)

	if err != nil {
		return nil, err
	}

	return ic, nil
}

func (wh *IngressControllerWebhook) authorized(request admissionctl.Request) admissionctl.Response {
	var ret admissionctl.Response
	ic, err := wh.renderIngressController(request)
	if err != nil {
		log.Error(err, "Couldn't render an IngressController from the incoming request")
		return admissionctl.Errored(http.StatusBadRequest, err)
	}

	log.Info("Checking if user is unauthenticated")
	if request.AdmissionRequest.UserInfo.Username == "system:unauthenticated" {
		// This could highlight a significant problem with RBAC since an
		// unauthenticated user should have no permissions.
		log.Info("system:unauthenticated made a webhook request. Check RBAC rules", "request", request.AdmissionRequest)
		ret = admissionctl.Denied("Unauthenticated")
		ret.UID = request.AdmissionRequest.UID
		return ret
	}

	log.Info("Checking if user is authenticated system: user")
	if strings.HasPrefix(request.AdmissionRequest.UserInfo.Username, "system:") {
		ret = admissionctl.Allowed("authenticated system: users are allowed")
		ret.UID = request.AdmissionRequest.UID
		return ret
	}

	log.Info("Checking if user is kube: user")
	if strings.HasPrefix(request.AdmissionRequest.UserInfo.Username, "kube:") {
		ret = admissionctl.Allowed("kube: users are allowed")
		ret.UID = request.AdmissionRequest.UID
		return ret
	}

	// Check if the group does not have exceptions
	if !isAllowedUser(request) {
		for _, toleration := range ic.Spec.NodePlacement.Tolerations {
			if strings.Contains(toleration.Key, "node-role.kubernetes.io/master") {
				ret = admissionctl.Denied("Not allowed to provision ingress controller pods with toleration for master nodes.")
				ret.UID = request.AdmissionRequest.UID

				return ret
			}
		}
	}

	reqOp := request.AdmissionRequest.Operation
	//TODO: ONLY check for privatelink clusters? ...any other cluster type this should apply to?
	// 		What other specifics should be checked here for this cidr check to be applicable?m
	// privateLink, err := someGetPrivateLinkFunction()
	// if !privateLink {

	// Only check for machine cidr in allowed ranges if creating or updating resource...
	if reqOp == admissionv1.Create || reqOp == admissionv1.Update {
		//TODO: Will these need to iterate over more than just the default IngressController config?
		if ic.ObjectMeta.Name == "default" && ic.ObjectMeta.Namespace == "openshift-ingress-operator" {
			//TODO: replace with machine cidr value provided by daemonset ...tbd...
			//machineCIDRString, err := someGetCIDRFunction()
			machineCIDRString := "10.0.0.0/16"
			log.Info("Matt: Warning using HARDCODED machine CIDR only use this code for TESTING!!!", "cidr", machineCIDRString)
			allowsMachineCidr, ret := wh.checkAllowsMachineCIDR(ic.Spec.EndpointPublishingStrategy.LoadBalancer.AllowedSourceRanges, machineCIDRString)
			if !allowsMachineCidr {
				log.Info("Error checking minimum AllowedSourceRange", "err", ret.AdmissionResponse.String())
				ret.UID = request.AdmissionRequest.UID
				return ret
			}
		}
	}
	// } // TODO: end privatelink and cluster checks

	log.Info("############# Matt LOG: IngressController operation is allowed ###########")
	ret = admissionctl.Allowed("IngressController operation is allowed")
	ret.UID = request.AdmissionRequest.UID

	return ret
}

func (wh *IngressControllerWebhook) checkAllowsMachineCIDR(ipRanges []operatorv1.CIDR, machineCIDRString string) (bool, admissionctl.Response) {
	// TODO: How is an empty list interpreted? allowing all or allowing no networks?
	// https://docs.openshift.com/container-platform/4.13/networking/configuring_ingress_cluster_traffic/configuring-ingress-cluster-traffic-load-balancer-allowed-source-ranges.html
	// From docs it appears a missing ASR value/attr allows all, but a deleted ASR can break things going forward.
	// For now return Allowed with a warning?
	if ipRanges == nil || len(ipRanges) <= 0 {
		return true, admissionctl.Allowed("Allowing empty 'AllowedSourceRanges'. Populate this value if operator remains in 'progressing' state")
		//return false, admissionctl.Denied("Spec.EndpointPublishingStrategy.LoadBalancer.AllowedSourceRanges must contain machine cidr")
	}
	if len(machineCIDRString) <= 0 {
		return false, admissionctl.Errored(http.StatusInternalServerError, fmt.Errorf("machine CIDR string not provided to AllowedSourceRanges min check. BUG!?! "))
	}
	machIP, machNet, err := net.ParseCIDR(string(machineCIDRString))
	if err != nil {
		return false, admissionctl.Errored(http.StatusInternalServerError, fmt.Errorf("failed to parse machine CIDR value: '%s'. Err: %s", string(machineCIDRString), err))
	}
	machNetSize, machNetBits := machNet.Mask.Size()
	log.Info(fmt.Sprintf("Checking masks. mach bits:'%d', machsize:%d", machNetBits, machNetSize))
	for _, ASRstring := range ipRanges {
		log.Info(fmt.Sprintf("Checking allowed source:'%s'", ASRstring))
		if len(ASRstring) <= 0 {
			continue
		}
		_, ASRNet, err := net.ParseCIDR(string(ASRstring))
		if err != nil {
			log.Info(fmt.Sprintf("failed to parse AllowedSourceRanges value: '%s'. Err: %s", string(ASRstring), err))
			return false, admissionctl.Errored(http.StatusBadRequest, fmt.Errorf("failed to parse AllowedSourceRanges value: '%s'. Err: %s", string(ASRstring), err))
		}
		// First check if AlloweSourceRange contains the machine cidr ip...
		if !ASRNet.Contains(machIP) {
			log.Info(fmt.Sprintf("AllowedSourceRange:'%s' does not contain machine CIDR:'%s'", ASRstring, machineCIDRString))
			continue
		}
		// Check if the mask includes the network.
		ASRNetSize, ASRNetBits := ASRNet.Mask.Size()
		log.Info(fmt.Sprintf("Checking masks. ASR bits:'%d', ASRsize:%d", ASRNetBits, ASRNetSize))
		if machNetBits == ASRNetBits && ASRNetSize <= machNetSize {
			log.Info(fmt.Sprintf("Found machineCidr:'%s' within AllowedSourceRange:'%s'", machineCIDRString, ASRstring))
			return true, admissionctl.Allowed("Minimum allowed source CIDR range checks met")
		}
		log.Info(fmt.Sprintf("NOT IN SUBNET machineCidr:'%s' within AllowedSourceRange:'%s'", machineCIDRString, ASRstring))
	}
	return false, admissionctl.Denied(fmt.Sprintf("At least one AllowedSourceRange must allow machine cidr:'%s'", machineCIDRString))
}

// isAllowedUser checks if the user is allowed to perform the action
func isAllowedUser(request admissionctl.Request) bool {
	log.Info(fmt.Sprintf("Checking username %s on whitelist", request.UserInfo.Username))
	if slices.Contains(allowedUsers, request.UserInfo.Username) {
		log.Info(fmt.Sprintf("%s is listed in whitelist", request.UserInfo.Username))
		return true
	}

	log.Info("No allowed user found")

	return false
}

// Authorized implements Webhook interface
func (wh *IngressControllerWebhook) Authorized(request admissionctl.Request) admissionctl.Response {
	return wh.authorized(request)
}

// SyncSetLabelSelector returns the label selector to use in the SyncSet.
// We turn on 'managed ingress v2' by setting legacy ingress to 'false'
// See https://github.com/openshift/cloud-ingress-operator/blob/master/hack/olm-registry/olm-artifacts-template.yaml
// and
// https://github.com/openshift/custom-domains-operator/blob/master/hack/olm-registry/olm-artifacts-template.yaml
// For examples of use.
func (s *IngressControllerWebhook) SyncSetLabelSelector() metav1.LabelSelector {
	customLabelSelector := utils.DefaultLabelSelector()
	customLabelSelector.MatchExpressions = append(customLabelSelector.MatchExpressions,
		metav1.LabelSelectorRequirement{
			Key:      legacyIngressSupportFeatureFlag,
			Operator: metav1.LabelSelectorOpIn,
			Values: []string{
				"false",
			},
		})
	return customLabelSelector
}

func (s *IngressControllerWebhook) ClassicEnabled() bool { return true }

func (s *IngressControllerWebhook) HypershiftEnabled() bool { return false }

// NewWebhook creates a new webhook
func NewWebhook() *IngressControllerWebhook {
	scheme := runtime.NewScheme()
	return &IngressControllerWebhook{
		s: *scheme,
	}
}
