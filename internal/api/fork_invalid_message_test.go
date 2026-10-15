package api

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestInvalidObjectMessageNamesRejectedFields(t *testing.T) {
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "core.orka.ai", Kind: "Task"}, "Bad_Name", field.ErrorList{
		field.Invalid(field.NewPath("metadata", "name"), "Bad_Name", "must be a lowercase RFC 1123 subdomain"),
		field.Required(field.NewPath("spec", "prompt"), ""),
	})
	require.Equal(t,
		`forked task is invalid: metadata.name: Invalid value: "Bad_Name": must be a lowercase RFC 1123 subdomain; spec.prompt: Required value`,
		invalidObjectMessage("forked task is invalid", invalid))
	require.Equal(t, "forked task is invalid", invalidObjectMessage("forked task is invalid", errors.New("not a status")))
}
