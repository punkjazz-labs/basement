package docredact

import (
	"reflect"
	"testing"
)

func TestExtractModelItems(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  []ModelItem
		ok    bool
	}{
		{"clean array", `[{"literal":"Mario Rossi","category":"person"}]`,
			[]ModelItem{{"Mario Rossi", "person"}}, true},
		{"prose around it", "Sure! Here you go:\n```json\n[{\"literal\":\"Acme S.p.A.\",\"category\":\"org\"}]\n```\nLet me know.",
			[]ModelItem{{"Acme S.p.A.", "org"}}, true},
		{"object wrapper", `{"items":[{"literal":"Via Roma 1, Milano","category":"address"}]}`,
			[]ModelItem{{"Via Roma 1, Milano", "address"}}, true},
		{"bracket inside string", `[{"literal":"pay [in full] to Mario","category":"amount"}]`,
			[]ModelItem{{"pay [in full] to Mario", "amount"}}, true},
		{"empty literal dropped", `[{"literal":"  ","category":"person"},{"literal":"Anna","category":"person"}]`,
			[]ModelItem{{"Anna", "person"}}, true},
		{"empty array is fine", `[]`, nil, true},
		{"garbage", `the document contains no sensitive data`, nil, false},
	}
	for _, c := range cases {
		got, err := ExtractModelItems(c.reply)
		if c.ok != (err == nil) {
			t.Fatalf("%s: err = %v", c.name, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: got %#v want %#v", c.name, got, c.want)
		}
	}
}
