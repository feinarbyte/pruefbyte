package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix prefixes every override, e.g. PRUEFBYTE_LLM_MODEL or PRUEFBYTE_REVIEW_MAX_COMMENTS.
const EnvPrefix = "PRUEFBYTE_"

var durationType = reflect.TypeOf(time.Duration(0))

// ApplyEnv overrides scalar and list fields from PRUEFBYTE_<SECTION>_<KEY> variables.
// Lists are comma-separated. Map fields (llm.extra_body, llm.extra_headers) and
// ocr.rules can only be set in a config file.
func (c *Config) ApplyEnv(lookup func(string) (string, bool)) error {
	return applyEnv(reflect.ValueOf(c).Elem(), EnvPrefix, lookup)
}

func applyEnv(v reflect.Value, prefix string, lookup func(string) (string, bool)) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		name := prefix + strings.ToUpper(tag)
		fv := v.Field(i)
		if f.Type.Kind() == reflect.Struct {
			if err := applyEnv(fv, name+"_", lookup); err != nil {
				return err
			}
			continue
		}
		raw, ok := lookup(name)
		if !ok {
			continue
		}
		if err := setFromString(fv, raw); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func setFromString(fv reflect.Value, raw string) error {
	if fv.Type() == durationType {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		fv.SetInt(int64(d))
		return nil
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Int:
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return err
		}
		fv.SetInt(int64(n))
	case reflect.Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return err
		}
		fv.SetBool(b)
	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.String {
			return nil
		}
		items := []string{}
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
		fv.Set(reflect.ValueOf(items))
	}
	return nil
}
