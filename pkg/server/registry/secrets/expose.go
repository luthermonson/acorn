package secrets

import (
	"github.com/acorn-io/mink/pkg/stores"
	"k8s.io/apiserver/pkg/registry/rest"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func NewExpose(c kclient.WithWatch) rest.Storage {
	return stores.NewGetOnly(NewStrategy(c, true))
}
