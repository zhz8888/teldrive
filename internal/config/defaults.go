package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/zhz8888/teldrive/v2/internal/size"
)

var (
	// durationType is the reflect type of time.Duration, used to recognize
	// fields whose default is written as a Go duration string such as "10s".
	durationType = reflect.TypeFor[time.Duration]()
	// sizeType is the reflect type of size.Size, used to recognize fields whose
	// default is written as a human-readable byte size such as "5MB".
	sizeType = reflect.TypeFor[size.Size]()
)

// applyDefaults fills every field of target that carries a `default` struct
// tag, descending into nested structs. The target must be a non-nil pointer to
// a struct. It returns an error naming the offending field path when a tag
// cannot be parsed into its field type or targets an unsupported type, and the
// fields visited before the failure keep their new values.
func applyDefaults(target any) error {
	value := reflect.ValueOf(target)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return fmt.Errorf("default target must be a non-nil pointer")
	}
	return applyStructDefaults(value.Elem(), "")
}

// applyStructDefaults walks the fields of one configuration struct and applies
// each `default` tag through setDefaultValue, recursing into nested structs and
// extending path with the field names so errors can point at a config key. It
// returns an error for a non-struct value or for the first default that cannot
// be parsed.
func applyStructDefaults(value reflect.Value, path string) error {
	if value.Kind() != reflect.Struct {
		return fmt.Errorf("default target %q must be a struct", path)
	}
	valueType := value.Type()
	for i := range value.NumField() {
		field := value.Field(i)
		fieldType := valueType.Field(i)
		fieldPath := fieldType.Name
		if path != "" {
			fieldPath = path + "." + fieldPath
		}
		if IsNestedStruct(fieldType.Type) {
			if err := applyStructDefaults(field, fieldPath); err != nil {
				return err
			}
			continue
		}
		raw, ok := fieldType.Tag.Lookup("default")
		if !ok {
			continue
		}
		if err := setDefaultValue(field, raw); err != nil {
			return fmt.Errorf("parse default for %s: %w", fieldPath, err)
		}
	}
	return nil
}

// setDefaultValue stores the raw `default` tag text into field, converting it
// according to the field type: durations and sizes are parsed from their string
// forms and the remaining kinds use the matching strconv parser. Slices are
// split on commas with each element trimmed. A whitespace-only value produces
// an empty slice or map, and a non-empty map default is rejected because maps
// can only default to empty. It returns an error when the field cannot be set,
// the value does not parse, or the type is unsupported.
func setDefaultValue(field reflect.Value, raw string) error {
	if !field.CanSet() {
		return fmt.Errorf("field cannot be set")
	}
	if field.Type() == durationType {
		value, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		field.SetInt(int64(value))
		return nil
	}
	if field.Type() == sizeType {
		value, err := size.Parse(raw)
		if err != nil {
			return err
		}
		field.SetInt(int64(value))
		return nil
	}
	switch field.Kind() {
	case reflect.String:
		field.SetString(raw)
	case reflect.Bool:
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		field.SetBool(value)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value, err := strconv.ParseInt(raw, 10, field.Type().Bits())
		if err != nil {
			return err
		}
		field.SetInt(value)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value, err := strconv.ParseUint(raw, 10, field.Type().Bits())
		if err != nil {
			return err
		}
		field.SetUint(value)
	case reflect.Slice:
		if field.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice type %s", field.Type())
		}
		if strings.TrimSpace(raw) == "" {
			field.Set(reflect.MakeSlice(field.Type(), 0, 0))
			return nil
		}
		parts := strings.Split(raw, ",")
		values := reflect.MakeSlice(field.Type(), len(parts), len(parts))
		for i, part := range parts {
			values.Index(i).SetString(strings.TrimSpace(part))
		}
		field.Set(values)
	case reflect.Map:
		if strings.TrimSpace(raw) != "" {
			return fmt.Errorf("non-empty map defaults are unsupported")
		}
		field.Set(reflect.MakeMap(field.Type()))
	default:
		return fmt.Errorf("unsupported field type %s", field.Type())
	}
	return nil
}
