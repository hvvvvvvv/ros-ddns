package logging

import (
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const (
	maxValueDepth = 32
	maxValueNodes = 10000
	truncated     = "[truncated]"
	unavailable   = "[unavailable]"
)

type visit struct {
	typ reflect.Type
	ptr uintptr
	len int
}

type normalizer struct {
	path      map[visit]bool
	remaining int
}

func newNormalizer() *normalizer {
	return &normalizer{path: make(map[visit]bool), remaining: maxValueNodes}
}

func (n *normalizer) attrs(attrs []slog.Attr, depth int) []slog.Attr {
	if depth > maxValueDepth {
		return []slog.Attr{slog.String("!value", truncated)}
	}
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if n.remaining <= 0 {
			out = append(out, slog.String("!value", truncated))
			break
		}
		n.remaining--
		value := a.Value.Resolve()
		if a.Key == "" && value.Kind() == slog.KindAny && value.Any() == nil {
			continue
		}
		if value.Kind() == slog.KindGroup {
			group := n.attrs(value.Group(), depth+1)
			if len(group) != 0 {
				out = append(out, slog.Attr{Key: a.Key, Value: slog.GroupValue(group...)})
			}
		} else {
			out = append(out, slog.Any(a.Key, n.value(reflect.ValueOf(value.Any()), depth+1)))
		}
	}
	return out
}

// value deliberately walks data instead of calling arbitrary JSON/Text marshalers
// or String methods. Errors and LogValuers are supported with panic recovery.
// Cycles, unsupported kinds and excessive depth/size become fixed strings, so
// neither output can panic while encoding or leak an unchecked formatting error.
func (n *normalizer) value(v reflect.Value, depth int) (out any) {
	defer func() {
		if recover() != nil {
			out = unavailable
		}
	}()
	if depth > maxValueDepth || n.remaining <= 0 {
		return truncated
	}
	n.remaining--
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		if v.IsNil() {
			return nil
		}
	}
	if v.CanInterface() {
		switch value := v.Interface().(type) {
		case time.Time:
			return value.Format(time.RFC3339Nano)
		case time.Duration:
			return value.String()
		case error:
			return value.Error()
		case slog.Value:
			return n.slogValue(value, depth+1)
		case slog.LogValuer:
			return n.slogValue(slog.AnyValue(value), depth+1)
		}
	}
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Map || v.Kind() == reflect.Slice {
		key := visit{typ: v.Type(), ptr: v.Pointer()}
		if v.Kind() == reflect.Slice {
			key.len = v.Len()
		}
		if n.path[key] {
			return "[cycle]"
		}
		n.path[key] = true
		defer delete(n.path, key)
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		return n.value(v.Elem(), depth+1)
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint()
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return strconv.FormatFloat(f, 'g', -1, 64)
		}
		return f
	case reflect.Map:
		values := make(map[string]any)
		iter := v.MapRange()
		for iter.Next() {
			if n.remaining <= 0 {
				values["!value"] = truncated
				break
			}
			values[mapKey(iter.Key())] = n.value(iter.Value(), depth+1)
		}
		return values
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			return string(v.Bytes())
		}
		values := make([]any, 0, min(v.Len(), n.remaining))
		for i := 0; i < v.Len(); i++ {
			if n.remaining <= 0 {
				values = append(values, truncated)
				break
			}
			values = append(values, n.value(v.Index(i), depth+1))
		}
		return values
	case reflect.Struct:
		values := make(map[string]any)
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if n.remaining <= 0 {
				values["!value"] = truncated
				break
			}
			if name == "" {
				name = field.Name
			}
			values[name] = n.value(v.Field(i), depth+1)
		}
		return values
	default:
		return "[unsupported]"
	}
}

func (n *normalizer) slogValue(value slog.Value, depth int) any {
	value = value.Resolve()
	if value.Kind() == slog.KindGroup {
		out := make(map[string]any)
		flattenAttrs(out, "", n.attrs(value.Group(), depth+1))
		return out
	}
	return n.value(reflect.ValueOf(value.Any()), depth+1)
}

func mapKey(key reflect.Value) string {
	if key.Kind() == reflect.Interface && !key.IsNil() {
		return mapKey(key.Elem())
	}
	switch key.Kind() {
	case reflect.String:
		return key.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(key.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(key.Uint(), 10)
	case reflect.Bool:
		return strconv.FormatBool(key.Bool())
	default:
		return "[unsupported key]"
	}
}
