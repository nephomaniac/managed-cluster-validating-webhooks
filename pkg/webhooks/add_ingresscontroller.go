package webhooks

import (
	"github.com/openshift/managed-cluster-validating-webhooks/pkg/webhooks/ingresscontroller"
)

func init() {
	ic := &ingresscontroller.IngressControllerWebhook{}
	if HypershiftEnabled && !ic.HypershiftEnabled() {
		return
	}
	Register(ingresscontroller.WebhookName, func() Webhook { return ingresscontroller.NewWebhook() })
}
