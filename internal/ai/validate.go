package ai

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"hhauto/internal/ai/schemas"
)

// validator holds the compiled strict schema of every task. Verdicts are
// ALWAYS validated locally — for every dialect — before the Runner returns
// them; OpenAI strict mode is an optimization, never a trust boundary.
type validator struct {
	compilers map[TaskName]*jsonschema.Schema
}

func newValidator() (*validator, error) {
	c := jsonschema.NewCompiler()
	v := &validator{compilers: map[TaskName]*jsonschema.Schema{}}
	for task := range taskNames {
		raw, err := schemas.FS.ReadFile(string(task) + ".json")
		if err != nil {
			return nil, fmt.Errorf("ai: schemas: %w", err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("ai: schemas: %s: %w", task, err)
		}
		if err := c.AddResource(string(task)+".json", doc); err != nil {
			return nil, fmt.Errorf("ai: schemas: %s: %w", task, err)
		}
	}
	for task := range taskNames {
		sch, err := c.Compile(string(task) + ".json")
		if err != nil {
			return nil, fmt.Errorf("ai: schemas: compile %s: %w", task, err)
		}
		v.compilers[task] = sch
	}
	return v, nil
}

// validate checks the raw verdict against the task schema.
func (v *validator) validate(task TaskName, raw json.RawMessage) error {
	sch, ok := v.compilers[task]
	if !ok {
		return fmt.Errorf("ai: schemas: no schema for task %q", task)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var inst any
	if err := dec.Decode(&inst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := sch.Validate(inst); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	return nil
}
