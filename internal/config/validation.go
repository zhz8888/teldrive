package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/go-playground/validator/v10"
)

// validateTaggedFields runs the `validate` struct tags over cfg and returns one
// message per failed rule, sorted so callers get stable output. Field paths are
// rendered from the `koanf` tag with the leading Config. prefix removed and the
// dots replaced by spaces, so a failure reads "http address failed required
// validation" rather than as a config key. A nil result means every tagged field
// passed.
func validateTaggedFields(cfg Config) []string {
	validate := validator.New(validator.WithRequiredStructEnabled())
	validate.RegisterTagNameFunc(func(field reflect.StructField) string {
		if key := field.Tag.Get("koanf"); key != "" && key != "-" {
			return key
		}
		return field.Name
	})
	if err := validate.Struct(cfg); err != nil {
		var problems []string
		if validationErrors, ok := err.(validator.ValidationErrors); ok {
			for _, fieldErr := range validationErrors {
				path := strings.TrimPrefix(fieldErr.Namespace(), "Config.")
				path = strings.ReplaceAll(path, ".", " ")
				problems = append(problems, fmt.Sprintf("%s failed %s validation", path, fieldErr.Tag()))
			}
		} else {
			problems = append(problems, err.Error())
		}
		slices.Sort(problems)
		return problems
	}
	return nil
}
