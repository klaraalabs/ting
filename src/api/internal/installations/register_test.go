package installations

import (
	"strings"
	"testing"

	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

func TestRegistrationRequiresDurableCredentialAndSafeGeneration(t *testing.T) {
	valid := Registration{Platform: "android", PushToken: "native-fcm-token", Token: secrets.Token("ting_ik_"), Generation: 1}
	for _, token := range []string{"", secrets.Token("ting_sk_"), "ting_ik_" + strings.Repeat("a", 42), "ting_ik_" + strings.Repeat("a", 43), valid.Token + "="} {
		r := valid
		r.Token = token
		if err := r.Validate(); err == nil {
			t.Fatal("accepted malformed or noncanonical installation credential")
		}
	}
	for _, generation := range []int64{0, -1, MaxGeneration + 1} {
		r := valid
		r.Generation = generation
		if err := r.Validate(); err == nil {
			t.Fatalf("accepted generation %d", generation)
		}
	}
	valid.Generation = MaxGeneration
	if err := valid.Validate(); err != nil {
		t.Fatalf("rejected largest cross-language safe generation: %v", err)
	}
}
