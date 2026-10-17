package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
)

// optionValue is the flag for one Option. It keeps the text the user typed, so
// numbers reach the API exactly as written.
type optionValue struct {
	typ  OptionType
	raw  string
	list []string
}

func (v *optionValue) Set(s string) error {
	switch v.typ {
	case typeString:
	case typeInteger:
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			return fmt.Errorf("%q is not an integer", s)
		}
	case typeNumber:
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return fmt.Errorf("%q is not a number", s)
		}
	case typeBoolean:
		if _, err := strconv.ParseBool(s); err != nil {
			return fmt.Errorf("%q is not true or false", s)
		}
	case typeStringList:
		v.list = append(v.list, strings.Split(s, ",")...)
	}
	v.raw = s
	return nil
}

func (v *optionValue) String() string {
	if v.typ == typeStringList {
		return strings.Join(v.list, ",")
	}
	return v.raw
}

// Type is the placeholder cobra shows in help. pflag treats "bool" as a switch.
func (v *optionValue) Type() string {
	switch v.typ {
	case typeInteger:
		return "int"
	case typeBoolean:
		return "bool"
	case typeStringList:
		return "strings"
	default:
		return string(v.typ)
	}
}

func (v *optionValue) jsonValue() any {
	switch v.typ {
	case typeInteger, typeNumber:
		return json.Number(v.raw)
	case typeBoolean:
		b, _ := strconv.ParseBool(v.raw)
		return b
	case typeStringList:
		return v.list
	default:
		return v.raw
	}
}

type boundOption struct {
	Option
	value *optionValue
	flag  *pflag.Flag
}

func (b boundOption) changed() bool { return b.flag.Changed }

func bindOption(flags *pflag.FlagSet, o Option) boundOption {
	usage := cmp.Or(o.Description, o.Field)
	if len(o.Enum) > 0 {
		usage += " (one of: " + strings.Join(o.Enum, ", ") + ")"
	}
	switch {
	case o.Required && o.In == inBody:
		usage += " (required, here or in --body)"
	case o.Required:
		usage += " (required)"
	}
	value := &optionValue{typ: o.Type}
	flag := flags.VarPF(value, o.Name, "", usage)
	if o.Type == typeBoolean {
		flag.NoOptDefVal = "true"
	}
	return boundOption{Option: o, value: value, flag: flag}
}
