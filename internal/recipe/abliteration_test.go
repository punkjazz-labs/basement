package recipe

import (
	"reflect"
	"testing"
)

func TestAbliterationVariantSharesStockWeightsAndPinsEveryDonor(t *testing.T) {
	recipes, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	stock, _ := Find(recipes, "glm53-flash-exl3-2s")
	variant, ok := Find(recipes, "glm53-flash-exl3-ablit-2s")
	if !ok || !variant.Runtime.Abliteration || stock.Runtime.Abliteration {
		t.Fatal("variant is not opt-in")
	}
	if !reflect.DeepEqual(stock.Artifacts[0], variant.Artifacts[0]) {
		t.Fatal("base artifact differs from stock")
	}
	if !reflect.DeepEqual(stock.Service, variant.Service) || !reflect.DeepEqual(stock.Topology, variant.Topology) {
		t.Fatal("variant changes serving or topology settings")
	}
	runtime := variant.Runtime
	runtime.Abliteration = false
	if !reflect.DeepEqual(stock.Runtime, runtime) {
		t.Fatal("variant changes runtime beyond ablation")
	}
	if len(variant.Artifacts) != 2 {
		t.Fatal("expected base and donor artifacts")
	}
	donor := variant.Artifacts[1]
	if donor.Role != "abliteration" || donor.ExpectedBytes != 2684354560 || len(donor.Files) != 31 {
		t.Fatalf("unexpected donor artifact: %+v", donor)
	}
	for _, f := range donor.Files {
		if f.Range == nil || f.Range.SHA256 == "" {
			t.Fatalf("donor not range pinned: %+v", f)
		}
	}
}
