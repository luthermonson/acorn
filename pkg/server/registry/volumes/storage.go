package volumes

import (
	"github.com/acorn-io/acorn/pkg/scheme"
	"github.com/acorn-io/mink/pkg/db"
	"github.com/acorn-io/mink/pkg/stores"
	"k8s.io/apiserver/pkg/registry/rest"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func NewStorage(c kclient.WithWatch, db *db.Factory) (rest.Storage, rest.Storage, error) {
	strategy, err := NewStrategy(c, db)
	if err != nil {
		return nil, nil, err
	}
	if db == nil {
		return stores.NewReadDelete(scheme.Scheme, strategy), nil, nil
	}
	stores, status := stores.NewWithStatus(c.Scheme(), strategy)
	return stores, status, nil
}
