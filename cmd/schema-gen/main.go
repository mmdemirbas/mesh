package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"
	"github.com/mmdemirbas/mesh/internal/config"
)

func main() {
	r := new(jsonschema.Reflector)
	r.FieldNameTag = "yaml"
	if err := r.AddGoComments("github.com/mmdemirbas/mesh", "../../internal/config"); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load Go comments: %v\n", err)
	}

	inner := r.Reflect(&config.Config{})

	// The YAML file is map[string]Config (node names → Config), not a bare
	// Config. Wrap so that every top-level key is validated as Config, while
	// x- prefixed YAML anchors are allowed with any value.
	schema := &jsonschema.Schema{
		Version:              inner.Version,
		ID:                   inner.ID,
		Type:                 "object",
		Definitions:          inner.Definitions,
		AdditionalProperties: &jsonschema.Schema{Ref: "#/$defs/Config"},
		PatternProperties: map[string]*jsonschema.Schema{
			"^x-": {},
		},
	}

	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshalling schema: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(string(b))
}
