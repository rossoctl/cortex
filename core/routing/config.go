package routing

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadRoutes loads routes from a YAML file.
// Returns an empty slice (not error) if the file doesn't exist.
func LoadRoutes(path string) ([]Route, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading routes config: %w", err)
	}
	var routes []Route
	if err := yaml.Unmarshal(data, &routes); err != nil {
		return nil, fmt.Errorf("parsing routes config: %w", err)
	}
	return routes, nil
}
