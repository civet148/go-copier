package copier

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// These flags define options for tag handling
const (
	// Denotes that a destination field must be copied to. If copying fails then a panic will ensue.
	tagMust uint8 = 1 << iota

	// Denotes that the program should not panic when the must flag is on and
	// value is not copied. The program will return an error instead.
	tagNoPanic

	// Ignore a destination field from being copied to.
	tagIgnore

	// Denotes the fact that the field should be overridden, no matter if the IgnoreEmpty is set
	tagOverride

	// Denotes that the value as been copied
	hasCopied

	// Some default converter types for a nicer syntax
	String  string  = ""
	Bool    bool    = false
	Int     int     = 0
	Float32 float32 = 0
	Float64 float64 = 0
)

// Option sets copy options
type Option struct {
	// setting this value to true will ignore copying zero values of all the fields, including bools, as well as a
	// struct having all it's fields set to their zero values respectively (see IsZero() in reflect/value.go)
	IgnoreEmpty   bool
	CaseSensitive bool
	DeepCopy      bool
	Converters    []TypeConverter
	// Custom field name mappings to copy values with different names in `fromValue` and `toValue` types.
	// Examples can be found in `copier_field_name_mapping_test.go`.
	FieldNameMapping []FieldNameMapping
	TimeLayout       string //time string layout
}

func (opt Option) converters() map[converterPair]TypeConverter {
	var converters = map[converterPair]TypeConverter{}

	// save converters into map for faster lookup
	for i := range opt.Converters {
		pair := converterPair{
			SrcType: reflect.TypeOf(opt.Converters[i].SrcType),
			DstType: reflect.TypeOf(opt.Converters[i].DstType),
		}

		converters[pair] = opt.Converters[i]
	}

	return converters
}

type TypeConverter struct {
	SrcType interface{}
	DstType interface{}
	Fn      func(src interface{}) (dst interface{}, err error)
}

type converterPair struct {
	SrcType reflect.Type
	DstType reflect.Type
}

func (opt Option) fieldNameMapping() map[converterPair]FieldNameMapping {
	var mapping = map[converterPair]FieldNameMapping{}

	for i := range opt.FieldNameMapping {
		pair := converterPair{
			SrcType: reflect.TypeOf(opt.FieldNameMapping[i].SrcType),
			DstType: reflect.TypeOf(opt.FieldNameMapping[i].DstType),
		}

		mapping[pair] = opt.FieldNameMapping[i]
	}

	return mapping
}

type FieldNameMapping struct {
	SrcType interface{}
	DstType interface{}
	Mapping map[string]string
}

// Tag Flags
type flags struct {
	BitFlags  map[string]uint8
	SrcNames  tagNameMapping
	DestNames tagNameMapping
}

// Field Tag name mapping
type tagNameMapping struct {
	FieldNameToTag map[string]string
	TagToFieldName map[string]string
}

type OptionFunc func(opt *Option)

func WithTimeLayout(layout string) OptionFunc {
	return func(opt *Option) {
		opt.TimeLayout = layout
	}
}

func WithDeepCopy() OptionFunc {
	return func(opt *Option) {
		opt.DeepCopy = true
	}
}

func WithCaseSensitive() OptionFunc {
	return func(opt *Option) {
		opt.CaseSensitive = true
	}
}

func WithConverters(converters []TypeConverter) OptionFunc {
	return func(opt *Option) {
		opt.Converters = converters
	}
}

func WithFieldNameMapping(fieldNameMapping []FieldNameMapping) OptionFunc {
	return func(opt *Option) {
		opt.FieldNameMapping = fieldNameMapping
	}
}

func WithIgnoreEmpty() OptionFunc {
	return func(opt *Option) {
		opt.IgnoreEmpty = true
	}
}

// Copy copy things
func Copy(toValue interface{}, fromValue interface{}, options ...OptionFunc) (err error) {
	var opt = Option{}
	for _, o := range options {
		o(&opt)
	}
	return copier(toValue, fromValue, opt)
}

// CopyCase copy things with case sensitive
func CopyCase(toValue interface{}, fromValue interface{}) (err error) {
	return copier(toValue, fromValue, Option{
		CaseSensitive: true,
	})
}

// CopyDeep copy things with deep copy
func CopyDeep(toValue interface{}, fromValue interface{}) (err error) {
	return copier(toValue, fromValue, Option{
		DeepCopy: true,
	})
}

// CopyCaseDeep copy things with case sensitive and deep copy
func CopyCaseDeep(toValue interface{}, fromValue interface{}) (err error) {
	return copier(toValue, fromValue, Option{
		DeepCopy:      true,
		CaseSensitive: true,
	})
}

// CopyWithOption copy with option
func CopyWithOption(toValue interface{}, fromValue interface{}, opt Option) (err error) {
	return copier(toValue, fromValue, opt)
}

func copier(toValue interface{}, fromValue interface{}, opt Option) (err error) {
	typ := reflect.TypeOf(toValue)
	if typ.Kind() == reflect.Ptr { //struct pointer address (&*Struct)
		typ = typ.Elem()
		val := reflect.ValueOf(toValue).Elem()
		switch typ.Kind() {
		case reflect.Ptr:
			{
				if typ.Elem().Kind() == reflect.Struct {
					if val.IsNil() {
						var typNew = typ.Elem()
						var valNew = reflect.New(typNew)
						val.Set(valNew)
					}
				}
			}
		}
	}

	var (
		isSlice    bool
		amount     = 1
		from       = indirect(reflect.ValueOf(fromValue))
		to         = indirect(reflect.ValueOf(toValue))
		converters = opt.converters()
		mappings   = opt.fieldNameMapping()
	)

	if !to.CanAddr() {
		return ErrInvalidCopyDestination
	}
	// Return is from value is invalid
	if !from.IsValid() {
		return ErrInvalidCopyFrom
	}

	fromType, isPtrFrom := indirectType(from.Type())
	toType, _ := indirectType(to.Type())

	if fromType.Kind() == reflect.Interface {
		fromType = reflect.TypeOf(from.Interface())
	}

	if toType.Kind() == reflect.Interface {
		toType, _ = indirectType(reflect.TypeOf(to.Interface()))
		oldTo := to
		to = reflect.New(reflect.TypeOf(to.Interface())).Elem()
		defer func() {
			oldTo.Set(to)
		}()
	}

	// Just set it if possible to assign for normal types
	if from.Kind() != reflect.Slice && from.Kind() != reflect.Struct && from.Kind() != reflect.Map && (from.Type().AssignableTo(to.Type()) || from.Type().ConvertibleTo(to.Type())) {
		if !isPtrFrom || !opt.DeepCopy {
			to.Set(from.Convert(to.Type()))
		} else {
			fromCopy := reflect.New(from.Type())
			fromCopy.Set(from.Elem())
			to.Set(fromCopy.Convert(to.Type()))
		}
		return
	}

	if from.Kind() != reflect.Slice && fromType.Kind() == reflect.Map && toType.Kind() == reflect.Map {
		if !fromType.Key().ConvertibleTo(toType.Key()) {
			return ErrMapKeyNotMatch
		}

		if to.IsNil() {
			to.Set(reflect.MakeMapWithSize(toType, from.Len()))
		}

		for _, k := range from.MapKeys() {
			toKey := indirect(reflect.New(toType.Key()))
			isSet, err := set(toKey, k, opt)
			if err != nil {
				return err
			}
			if !isSet {
				return fmt.Errorf("%w map, old key: %v, new key: %v", ErrNotSupported, k.Type(), toType.Key())
			}

			elemType := toType.Elem()
			if elemType.Kind() != reflect.Slice {
				elemType, _ = indirectType(elemType)
			}
			toValue := indirect(reflect.New(elemType))
			isSet, err = set(toValue, from.MapIndex(k), opt)
			if err != nil {
				return err
			}
			if !isSet {
				if err = copier(toValue.Addr().Interface(), from.MapIndex(k).Interface(), opt); err != nil {
					return err
				}
			}

			for {
				if elemType == toType.Elem() {
					to.SetMapIndex(toKey, toValue)
					break
				}
				elemType = reflect.PointerTo(elemType)
				toValue = toValue.Addr()
			}
		}
		return
	}

	if from.Kind() == reflect.Slice && to.Kind() == reflect.Slice {
		// Return directly if both slices are nil
		if from.IsNil() && to.IsNil() {
			return
		}

		if fromType.ConvertibleTo(toType) {
			if to.IsNil() {
				slice := reflect.MakeSlice(reflect.SliceOf(to.Type().Elem()), from.Len(), from.Cap())
				to.Set(slice)
			}
			for i := 0; i < from.Len(); i++ {
				if to.Len() < i+1 {
					to.Set(reflect.Append(to, reflect.New(to.Type().Elem()).Elem()))
				}
				isSet, err := set(to.Index(i), from.Index(i), opt)
				if err != nil {
					return err
				}
				if !isSet {
					// ignore error while copy slice element
					err = copier(to.Index(i).Addr().Interface(), from.Index(i).Interface(), opt)
					if err != nil {
						continue
					}
				}
			}

			if to.Len() > from.Len() {
				to.SetLen(from.Len())
			}

			return
		} else {
			ok, _ := copyBaseSlice(from, to)
			if ok {
				return
			}
		}
	}

	if fromType.Kind() != reflect.Struct || toType.Kind() != reflect.Struct {
		// skip not supported type
		return
	}

	if len(converters) > 0 {
		if ok, e := set(to, from, opt); e == nil && ok {
			// converter supported
			return
		}
	}

	if from.Kind() == reflect.Slice || to.Kind() == reflect.Slice {
		isSlice = true
		if from.Kind() == reflect.Slice {
			amount = from.Len()
		}
	}

	for i := 0; i < amount; i++ {
		var dest, source reflect.Value

		if isSlice {
			// source
			if from.Kind() == reflect.Slice {
				source = indirect(from.Index(i))
			} else {
				source = indirect(from)
			}
			// dest
			dest = indirect(reflect.New(toType).Elem())
		} else {
			source = indirect(from)
			dest = indirect(to)
		}

		if len(converters) > 0 {
			if ok, e := set(dest, source, opt); e == nil && ok {
				if isSlice {
					// FIXME: maybe should check the other types?
					if to.Type().Elem().Kind() == reflect.Ptr {
						to.Index(i).Set(dest.Addr())
					} else {
						if to.Len() < i+1 {
							reflect.Append(to, dest)
						} else {
							to.Index(i).Set(dest)
						}
					}
				} else {
					to.Set(dest)
				}

				continue
			}
		}

		destKind := dest.Kind()
		initDest := false
		if destKind == reflect.Interface {
			initDest = true
			dest = indirect(reflect.New(toType))
		}

		// Get tag options
		flgs, err := getFlags(dest, source, toType, fromType)
		if err != nil {
			return err
		}

		// check source
		if source.IsValid() {
			copyUnexportedStructFields(dest, source)

			// Copy from source field to dest field or method
			fromTypeFields := deepFields(fromType)
			for _, field := range fromTypeFields {
				name := field.Name

				// Get bit flags for field
				fieldFlags := flgs.BitFlags[name]

				// Check if we should ignore copying
				if (fieldFlags & tagIgnore) != 0 {
					continue
				}

				fieldNamesMapping := getFieldNamesMapping(mappings, fromType, toType)

				srcFieldName, destFieldName := getFieldName(name, flgs, fieldNamesMapping)

				if fromField := fieldByNameOrZeroValue(source, srcFieldName); fromField.IsValid() && !shouldIgnore(fromField, fieldFlags, opt.IgnoreEmpty) {
					// process for nested anonymous field
					destFieldNotSet := false
					if f, ok := dest.Type().FieldByName(destFieldName); ok {
						// only initialize parent embedded struct pointer in the path
						for idx := range f.Index[:len(f.Index)-1] {
							destField := dest.FieldByIndex(f.Index[:idx+1])

							if destField.Kind() != reflect.Ptr {
								continue
							}

							if !destField.IsNil() {
								continue
							}
							if !destField.CanSet() {
								destFieldNotSet = true
								break
							}

							// destField is a nil pointer that can be set
							newValue := reflect.New(destField.Type().Elem())
							destField.Set(newValue)
						}
					}

					if destFieldNotSet {
						break
					}

					toField := fieldByName(dest, destFieldName, opt.CaseSensitive)
					if toField.IsValid() {
						if toField.CanSet() {
							isSet, err := set(toField, fromField, opt)
							if err != nil {
								return err
							}
							if !isSet {
								if err := copier(toField.Addr().Interface(), fromField.Interface(), opt); err != nil {
									return err
								}
							}
							if fieldFlags != 0 {
								// Note that a copy was made
								flgs.BitFlags[name] = fieldFlags | hasCopied
							}
						}
					} else {
						// try to set to method
						var toMethod reflect.Value
						if dest.CanAddr() {
							toMethod = dest.Addr().MethodByName(destFieldName)
						} else {
							toMethod = dest.MethodByName(destFieldName)
						}

						if toMethod.IsValid() && toMethod.Type().NumIn() == 1 && fromField.Type().AssignableTo(toMethod.Type().In(0)) {
							toMethod.Call([]reflect.Value{fromField})
						}
					}
				}
			}

			// Copy from from method to dest field
			for _, field := range deepFields(toType) {
				name := field.Name
				srcFieldName, destFieldName := getFieldName(name, flgs, getFieldNamesMapping(mappings, fromType, toType))

				var fromMethod reflect.Value
				if source.CanAddr() {
					fromMethod = source.Addr().MethodByName(srcFieldName)
				} else {
					fromMethod = source.MethodByName(srcFieldName)
				}

				if fromMethod.IsValid() && fromMethod.Type().NumIn() == 0 && fromMethod.Type().NumOut() == 1 && !shouldIgnore(fromMethod, flgs.BitFlags[name], opt.IgnoreEmpty) {
					if toField := fieldByName(dest, destFieldName, opt.CaseSensitive); toField.IsValid() && toField.CanSet() {
						values := fromMethod.Call([]reflect.Value{})
						if len(values) >= 1 {
							set(toField, values[0], opt)
						}
					}
				}
			}
		}

		if isSlice && to.Kind() == reflect.Slice {
			if dest.Addr().Type().AssignableTo(to.Type().Elem()) {
				if to.Len() < i+1 {
					to.Set(reflect.Append(to, dest.Addr()))
				} else {
					isSet, err := set(to.Index(i), dest.Addr(), opt)
					if err != nil {
						return err
					}
					if !isSet {
						// ignore error while copy slice element
						err = copier(to.Index(i).Addr().Interface(), dest.Addr().Interface(), opt)
						if err != nil {
							continue
						}
					}
				}
			} else if dest.Type().AssignableTo(to.Type().Elem()) {
				if to.Len() < i+1 {
					to.Set(reflect.Append(to, dest))
				} else {
					isSet, err := set(to.Index(i), dest, opt)
					if err != nil {
						return err
					}
					if !isSet {
						// ignore error while copy slice element
						err = copier(to.Index(i).Addr().Interface(), dest.Interface(), opt)
						if err != nil {
							continue
						}
					}
				}
			}
		} else if initDest {
			to.Set(dest)
		}

		err = checkBitFlags(flgs.BitFlags)
	}

	return
}

func getFieldNamesMapping(mappings map[converterPair]FieldNameMapping, fromType reflect.Type, toType reflect.Type) map[string]string {
	var fieldNamesMapping map[string]string

	if len(mappings) > 0 {
		pair := converterPair{
			SrcType: fromType,
			DstType: toType,
		}
		if v, ok := mappings[pair]; ok {
			fieldNamesMapping = v.Mapping
		}
	}
	return fieldNamesMapping
}

func fieldByNameOrZeroValue(source reflect.Value, fieldName string) (value reflect.Value) {
	defer func() {
		if err := recover(); err != nil {
			value = reflect.Value{}
		}
	}()

	return source.FieldByName(fieldName)
}

func copyUnexportedStructFields(to, from reflect.Value) {
	if from.Kind() != reflect.Struct || to.Kind() != reflect.Struct || !from.Type().AssignableTo(to.Type()) {
		return
	}

	// create a shallow copy of 'to' to get all fields
	tmp := indirect(reflect.New(to.Type()))
	tmp.Set(from)

	// revert exported fields
	for i := 0; i < to.NumField(); i++ {
		if tmp.Field(i).CanSet() {
			tmp.Field(i).Set(to.Field(i))
		}
	}
	to.Set(tmp)
}

func shouldIgnore(v reflect.Value, bitFlags uint8, ignoreEmpty bool) bool {
	return ignoreEmpty && bitFlags&tagOverride == 0 && v.IsZero()
}

var deepFieldsLock sync.RWMutex
var deepFieldsMap = make(map[reflect.Type][]reflect.StructField)

func deepFields(reflectType reflect.Type) []reflect.StructField {
	deepFieldsLock.RLock()
	cache, ok := deepFieldsMap[reflectType]
	deepFieldsLock.RUnlock()
	if ok {
		return cache
	}
	var res []reflect.StructField
	if reflectType, _ = indirectType(reflectType); reflectType.Kind() == reflect.Struct {
		fields := make([]reflect.StructField, 0, reflectType.NumField())

		for i := 0; i < reflectType.NumField(); i++ {
			v := reflectType.Field(i)
			// PkgPath is the package path that qualifies a lower case (unexported)
			// field name. It is empty for upper case (exported) field names.
			// See https://golang.org/ref/spec#Uniqueness_of_identifiers
			if v.PkgPath == "" {
				fields = append(fields, v)
				if v.Anonymous {
					// also consider fields of anonymous fields as fields of the root
					fields = append(fields, deepFields(v.Type)...)
				}
			}
		}
		res = fields
	}

	deepFieldsLock.Lock()
	deepFieldsMap[reflectType] = res
	deepFieldsLock.Unlock()
	return res
}

func indirect(reflectValue reflect.Value) reflect.Value {
	for reflectValue.Kind() == reflect.Ptr {
		reflectValue = reflectValue.Elem()
	}
	return reflectValue
}

func indirectType(reflectType reflect.Type) (_ reflect.Type, isPtr bool) {
	for reflectType.Kind() == reflect.Ptr || reflectType.Kind() == reflect.Slice {
		reflectType = reflectType.Elem()
		isPtr = true
	}
	return reflectType, isPtr
}

func set(to, from reflect.Value, opt Option) (bool, error) {
	deepCopy := opt.DeepCopy
	converters := opt.converters()
	if !from.IsValid() {
		return true, nil
	}
	if ok, err := lookupAndCopyWithConverter(to, from, converters); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}

	if to.Kind() == reflect.Ptr {
		// set `to` to nil if from is nil
		if from.Kind() == reflect.Ptr && from.IsNil() {
			to.Set(reflect.Zero(to.Type()))
			return true, nil
		} else if to.IsNil() {
			// `from`         -> `to`
			// sql.NullString -> *string
			if fromValuer, ok := driverValuer(from); ok {
				v, err := fromValuer.Value()
				if err != nil {
					return true, nil
				}
				// if `from` is not valid do nothing with `to`
				if v == nil {
					return true, nil
				}
			}
			// allocate new `to` variable with default value (eg. *string -> new(string))
			if isPtrTimeType(to.Type()) {
				if from.CanInt() {
					if from.Int() == 0 {
						return true, nil
					}
				} else if from.CanInterface() {
					var strValue = fmt.Sprintf("%v", from.Interface())
					if strings.TrimSpace(strValue) == "" {
						return true, nil
					}
				}
			}
			to.Set(reflect.New(to.Type().Elem()))
		} else if from.Kind() != reflect.Ptr && from.IsZero() {
			to.Set(reflect.Zero(to.Type()))
			return true, nil
		}
		// depointer `to`
		to = to.Elem()
	}

	if deepCopy {
		toKind := to.Kind()
		if toKind == reflect.Interface && to.IsNil() {
			if reflect.TypeOf(from.Interface()) != nil {
				to.Set(reflect.New(reflect.TypeOf(from.Interface())).Elem())
				toKind = reflect.TypeOf(to.Interface()).Kind()
			}
		}
		if from.Kind() == reflect.Ptr && from.IsNil() {
			to.Set(reflect.Zero(to.Type()))
			return true, nil
		}
		if _, ok := to.Addr().Interface().(sql.Scanner); !ok && (toKind == reflect.Struct || toKind == reflect.Map || toKind == reflect.Slice) {
			return false, nil
		}
	}

	// try convert directly
	if from.Type().ConvertibleTo(to.Type()) {
		if isString(from) && isNumber(to) {
			return setStringToNumber(from, to)
		} else if isNumber(from) && isString(to) {
			return setNumberToString(from, to)
		} else {
			to.Set(from.Convert(to.Type()))
		}
		return true, nil
	} else {
		if isString(from) && isNumber(to) {
			return setStringToNumber(from, to)
		} else if isNumber(from) && isString(to) {
			return setNumberToString(from, to)
		} else if isTime(from) && (isNumber(to) || isString(to)) {
			return setTimeToNumberOrString(from, to, opt)
		} else if (isNumber(from) || isString(from)) && isTime(to) {
			return setNumberOrStringToTime(from, to, opt)
		} else {
			ok, _ := copyBaseSlice(from, to)
			if ok {
				return true, nil
			}
		}
	}

	// try Scanner
	if toScanner, ok := to.Addr().Interface().(sql.Scanner); ok {
		// `from`  -> `to`
		// *string -> sql.NullString
		if from.Kind() == reflect.Ptr {
			// if `from` is nil do nothing with `to`
			if from.IsNil() {
				return true, nil
			}
			// depointer `from`
			from = indirect(from)
		}
		// `from` -> `to`
		// string -> sql.NullString
		// set `to` by invoking method Scan(`from`)
		err := toScanner.Scan(from.Interface())
		if err == nil {
			return true, nil
		}
	}

	// try Valuer
	if fromValuer, ok := driverValuer(from); ok {
		// `from`         -> `to`
		// sql.NullString -> string
		v, err := fromValuer.Value()
		if err != nil {
			return false, nil
		}
		// if `from` is not valid do nothing with `to`
		if v == nil {
			return true, nil
		}
		rv := reflect.ValueOf(v)
		if rv.Type().AssignableTo(to.Type()) {
			to.Set(rv)
			return true, nil
		}
		if to.CanSet() && rv.Type().ConvertibleTo(to.Type()) {
			to.Set(rv.Convert(to.Type()))
			return true, nil
		}
		return false, nil
	}

	// from is ptr
	if from.Kind() == reflect.Ptr {
		return set(to, from.Elem(), opt)
	}

	return false, nil
}

// lookupAndCopyWithConverter looks up the type pair, on success the TypeConverter Fn func is called to copy src to dst field.
func lookupAndCopyWithConverter(to, from reflect.Value, converters map[converterPair]TypeConverter) (copied bool, err error) {
	pair := converterPair{
		SrcType: from.Type(),
		DstType: to.Type(),
	}

	if cnv, ok := converters[pair]; ok {
		result, err := cnv.Fn(from.Interface())
		if err != nil {
			return false, err
		}

		if result != nil {
			to.Set(reflect.ValueOf(result))
		} else {
			// in case we've got a nil value to copy
			to.Set(reflect.Zero(to.Type()))
		}

		return true, nil
	}

	return false, nil
}

// parseTags Parses struct tags and returns uint8 bit flags.
func parseTags(tag string) (flg uint8, name string, err error) {
	for _, t := range strings.Split(tag, ",") {
		switch t {
		case "-":
			flg = tagIgnore
			return
		case "must":
			flg = flg | tagMust
		case "nopanic":
			flg = flg | tagNoPanic
		case "override":
			flg = flg | tagOverride
		default:
			if unicode.IsUpper([]rune(t)[0]) {
				name = strings.TrimSpace(t)
			} else {
				err = ErrFieldNameTagStartNotUpperCase
			}
		}
	}
	return
}

// getTagFlags Parses struct tags for bit flags, field name.
func getFlags(dest, src reflect.Value, toType, fromType reflect.Type) (flags, error) {
	flgs := flags{
		BitFlags: map[string]uint8{},
		SrcNames: tagNameMapping{
			FieldNameToTag: map[string]string{},
			TagToFieldName: map[string]string{},
		},
		DestNames: tagNameMapping{
			FieldNameToTag: map[string]string{},
			TagToFieldName: map[string]string{},
		},
	}

	var toTypeFields, fromTypeFields []reflect.StructField
	if dest.IsValid() {
		toTypeFields = deepFields(toType)
	}
	if src.IsValid() {
		fromTypeFields = deepFields(fromType)
	}

	// Get a list dest of tags
	for _, field := range toTypeFields {
		tags := field.Tag.Get("copier")
		if tags != "" {
			var name string
			var err error
			if flgs.BitFlags[field.Name], name, err = parseTags(tags); err != nil {
				return flags{}, err
			} else if name != "" {
				flgs.DestNames.FieldNameToTag[field.Name] = name
				flgs.DestNames.TagToFieldName[name] = field.Name
			}
		}
	}

	// Get a list source of tags
	for _, field := range fromTypeFields {
		tags := field.Tag.Get("copier")
		if tags != "" {
			var name string
			var err error

			if _, name, err = parseTags(tags); err != nil {
				return flags{}, err
			} else if name != "" {
				flgs.SrcNames.FieldNameToTag[field.Name] = name
				flgs.SrcNames.TagToFieldName[name] = field.Name
			}
		}
	}

	return flgs, nil
}

// checkBitFlags Checks flags for error or panic conditions.
func checkBitFlags(flagsList map[string]uint8) (err error) {
	// Check flag conditions were met
	for name, flgs := range flagsList {
		if flgs&hasCopied == 0 {
			switch {
			case flgs&tagMust != 0 && flgs&tagNoPanic != 0:
				err = fmt.Errorf("field %s has must tag but was not copied", name)
				return
			case flgs&(tagMust) != 0:
				panic(fmt.Sprintf("Field %s has must tag but was not copied", name))
			}
		}
	}
	return
}

func getFieldName(fieldName string, flgs flags, fieldNameMapping map[string]string) (srcFieldName string, destFieldName string) {
	// get dest field name
	if name, ok := fieldNameMapping[fieldName]; ok {
		srcFieldName = fieldName
		destFieldName = name
		return
	}

	if srcTagName, ok := flgs.SrcNames.FieldNameToTag[fieldName]; ok {
		destFieldName = srcTagName
		if destTagName, ok := flgs.DestNames.TagToFieldName[srcTagName]; ok {
			destFieldName = destTagName
		}
	} else {
		if destTagName, ok := flgs.DestNames.TagToFieldName[fieldName]; ok {
			destFieldName = destTagName
		}
	}
	if destFieldName == "" {
		destFieldName = fieldName
	}

	// get source field name
	if destTagName, ok := flgs.DestNames.FieldNameToTag[fieldName]; ok {
		srcFieldName = destTagName
		if srcField, ok := flgs.SrcNames.TagToFieldName[destTagName]; ok {
			srcFieldName = srcField
		}
	} else {
		if srcField, ok := flgs.SrcNames.TagToFieldName[fieldName]; ok {
			srcFieldName = srcField
		}
	}

	if srcFieldName == "" {
		srcFieldName = fieldName
	}
	return
}

func driverValuer(v reflect.Value) (i driver.Valuer, ok bool) {
	if !v.CanAddr() {
		i, ok = v.Interface().(driver.Valuer)
		return
	}

	i, ok = v.Addr().Interface().(driver.Valuer)
	return
}

func fieldByName(v reflect.Value, name string, caseSensitive bool) reflect.Value {
	if caseSensitive {
		return v.FieldByName(name)
	}
	return v.FieldByNameFunc(func(n string) bool {
		if len(n) != 0 {
			c := rune(n[0])
			if !unicode.IsUpper(c) {
				return false //exclude unexported fields
			}
		}
		return strings.EqualFold(n, name)
	})
}

func isNumber(val reflect.Value) bool {
	switch val.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

func isInteger(val reflect.Value) bool {
	switch val.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

func isFloat(val reflect.Value) bool {
	switch val.Kind() {
	case reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

func isString(val reflect.Value) bool {
	switch val.Kind() {
	case reflect.String:
		return true
	}
	return false
}

func isStringOrIntSlice(val reflect.Value) bool {
	switch val.Kind() {
	case reflect.Slice:
		typ := reflect.TypeOf(val.Interface())
		elemTyp := typ.Elem()
		if elemTyp.Kind() == reflect.Ptr {
			elemTyp = elemTyp.Elem()
		}
		switch elemTyp.Kind() {
		case reflect.Struct, reflect.Map, reflect.Array, reflect.Chan, reflect.Func, reflect.Ptr, reflect.Slice:
			return false
		}
		return true
	}
	return false
}

func isTime(val reflect.Value) bool {
	// 检查值是否有效且可导出
	if !val.IsValid() || !val.CanInterface() {
		return false
	}
	// 尝试类型断言
	_, ok := val.Interface().(time.Time)
	return ok
}

func setNumberToString(from, to reflect.Value) (bool, error) {
	val := fmt.Sprintf("%v", from.Interface())
	to.SetString(val)
	return true, nil
}

func setTimeToNumberOrString(from, to reflect.Value, opt Option) (bool, error) {
	var t = from.Interface().(time.Time)
	val := t.Unix()
	if isInteger(to) {
		switch to.Kind() {
		case reflect.Int, reflect.Int32, reflect.Int64:
			to.SetInt(val)
		case reflect.Uint32, reflect.Uint64:
			to.SetUint(uint64(val))
		}
	} else if isString(to) {
		if opt.TimeLayout != "" {
			to.SetString(t.Format(opt.TimeLayout))
		} else {
			to.SetString(t.Format(time.DateTime))
		}
	} else {
		return false, nil
	}
	return true, nil
}

func setNumberOrStringToTime(from, to reflect.Value, opt Option) (bool, error) {

	var val64 int64
	if isInteger(from) {
		val := from.Interface()
		switch from.Kind() {
		case reflect.Int:
			val64 = int64(val.(int))
		case reflect.Int32:
			val64 = int64(val.(int32))
		case reflect.Int64:
			val64 = val.(int64)
		case reflect.Uint:
			val64 = int64(val.(uint))
		case reflect.Uint32:
			val64 = int64(val.(uint32))
		case reflect.Uint64:
			val64 = int64(val.(uint64))
		}
	} else if isString(from) {
		var layout = opt.TimeLayout
		if layout == "" {
			layout = time.DateTime
		}
		tm, err := time.ParseInLocation(layout, from.String(), time.Local)
		if err != nil {
			return false, nil
		}
		val64 = tm.Unix()
	}
	if t, ok := to.Interface().(time.Time); ok {
		t = time.Unix(val64, 0)
		to.Set(reflect.ValueOf(t))
	}
	return true, nil
}

func setStringToNumber(from, to reflect.Value) (bool, error) {
	val := fmt.Sprintf("%v", from.Interface())
	if isInteger(to) {
		valInt64, _ := strconv.ParseInt(val, 10, 64)
		valUint64, _ := strconv.ParseUint(val, 10, 64)
		switch to.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			to.SetInt(valInt64)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			to.SetUint(valUint64)
		default:
		}
	} else if isFloat(to) {
		val64, _ := strconv.ParseFloat(val, 64)
		to.SetFloat(val64)
	}
	return true, nil
}

func copyBaseSlice(from, to reflect.Value) (bool, error) {
	if isStringOrIntSlice(from) && isStringOrIntSlice(to) {
		if to.IsNil() {
			to.Set(reflect.MakeSlice(to.Type(), 0, 0)) //make slice for storage
		}
		var toElemVal reflect.Value
		var toElemTyp reflect.Type

		toElemTyp = to.Type().Elem()
		for i := 0; i < from.Len(); i++ {
			var fromElemVal reflect.Value
			fromElemVal = from.Index(i)
			if toElemTyp.Kind() == reflect.Ptr {
				toElemVal = reflect.New(toElemTyp.Elem())
			} else {
				toElemVal = reflect.New(toElemTyp).Elem()
			}
			if isString(fromElemVal) && isNumber(toElemVal) {
				_, err := setStringToNumber(fromElemVal, toElemVal)
				if err != nil {
					return false, err
				}
			} else if isNumber(fromElemVal) && isString(toElemVal) {
				_, err := setNumberToString(fromElemVal, toElemVal)
				if err != nil {
					return false, err
				}
			}
			to.Set(reflect.Append(to, toElemVal))
		}
		return true, nil
	}
	return false, nil
}

func isPtrTimeType(t reflect.Type) bool {
	// 检查是否为指针类型
	if t.Kind() != reflect.Ptr {
		return false
	}

	// 获取指针指向的类型
	elemType := t.Elem()

	// 检查指向的类型是否为 time.Time
	return elemType == reflect.TypeOf(time.Time{})
}
