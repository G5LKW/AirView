package server

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"airview/internal/model"
)

// TestPutConfigKeepsEverySection: apiPutConfig must keep every top-level
// section of model.Config that a PUT body does not send (the web settings
// page does not know every block, e.g. ssr, locate, update). A new
// top-level block without a has("...") line would be reset by every save.
func TestPutConfigKeepsEverySection(t *testing.T) {
	src, err := os.ReadFile("api_core.go")
	if err != nil {
		t.Fatal(err)
	}
	ty := reflect.TypeOf(model.Config{})
	for i := 0; i < ty.NumField(); i++ {
		tag := strings.Split(ty.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		if !strings.Contains(string(src), `has("`+tag+`")`) {
			t.Errorf("PUT /api/config does not keep the unsent section %q", tag)
		}
	}
}
