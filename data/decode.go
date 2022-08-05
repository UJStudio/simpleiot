package data

import (
	"reflect"
)

// Decode converts a Node to custom struct.
// output must be a struct type that contains
// node, point, and edgepoint tags as shown below.
// It is recommended that id and parent node tags
// always be included.
//	   type exType struct {
//		ID          string  `node:"id"`
//		Parent      string  `node:"parent"`
//		Description string  `point:"description"`
//		Count       int     `point:"count"`
//		Role        string  `edgepoint:"role"`
//		Tombstone   bool    `edgepoint:"tombstone"`
//	   }
func Decode(input NodeEdge, output interface{}) error {
	vOut := reflect.ValueOf(output).Elem()
	tOut := reflect.TypeOf(output).Elem()

	pointValues := make(map[string]reflect.Value)
	edgeValues := make(map[string]reflect.Value)

	for i := 0; i < tOut.NumField(); i++ {
		sf := tOut.Field(i)
		if pt := sf.Tag.Get("point"); pt != "" {
			pointValues[pt] = vOut.Field(i)
		} else if et := sf.Tag.Get("edgepoint"); et != "" {
			edgeValues[et] = vOut.Field(i)
		} else if nt := sf.Tag.Get("node"); nt != "" {
			if nt == "id" {
				vOut.Field(i).SetString(input.ID)
			} else if nt == "parent" {
				vOut.Field(i).SetString(input.Parent)
			}
		}
	}

	setVal := func(p Point, v reflect.Value) {
		switch v.Type().Kind() {
		case reflect.String:
			v.SetString(p.Text)
		case reflect.Int:
			v.SetInt(int64(p.Value))
		case reflect.Float64, reflect.Float32:
			v.SetFloat(p.Value)
		case reflect.Bool:
			v.SetBool(FloatToBool(p.Value))
		}
	}

	for _, p := range input.Points {
		v, ok := pointValues[p.Type]
		if ok {
			setVal(p, v)
		}
	}

	for _, p := range input.EdgePoints {
		v, ok := edgeValues[p.Type]
		if ok {
			setVal(p, v)
		}
	}

	return nil
}