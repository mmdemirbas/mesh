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
	// We map the root type Config as the entrypoint for the JSON schema.
	if err := r.AddGoComments("github.com/mmdemirbas/mesh", "./internal/config"); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load Go comments: %v\n", err)
	}

	schema := r.Reflect(&config.Config{})

	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshalling schema: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(string(b))
}
