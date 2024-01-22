// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build ignore
// +build ignore

package main

import (
	"bytes"
	"flag"
	"go/format"
	"log"
	"os"
	"path/filepath"
	"text/template"

	. "github.com/pingcap/tidb/pkg/expression/generator/helper"
)

const header = `// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by go generate in expression/generator; DO NOT EDIT.

package expression
`

const newLine = "\n"

const builtinCompareImports = `import (
	"cmp"

	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
)
`

var builtinCompareVecTpl = template.Must(template.New("").Parse(`
func (b *builtin{{ .compare.CompareName }}{{ .type.TypeName }}Sig) vecEvalInt(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	buf0, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(buf0)
	if err := b.args[0].VecEval{{ .type.TypeName }}(ctx, input, buf0); err != nil {
		return err
	}
	buf1, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(buf1)
	if err := b.args[1].VecEval{{ .type.TypeName }}(ctx, input, buf1); err != nil {
		return err
	}

{{ if .type.Fixed }}
	arg0 := buf0.{{ .type.TypeNameInColumn }}s()
	arg1 := buf1.{{ .type.TypeNameInColumn }}s()
{{- end }}
	result.ResizeInt64(n, false)
	result.MergeNulls(buf0, buf1)
	i64s := result.Int64s()
	for i := 0; i < n; i++ {
		if result.IsNull(i) {
			continue
		}
{{- if eq .type.ETName "Json" }}
		val := types.CompareBinaryJSON(buf0.GetJSON(i), buf1.GetJSON(i))
{{- else if eq .type.ETName "Real" }}
		val := cmp.Compare(arg0[i], arg1[i])
{{- else if eq .type.ETName "String" }}
		val := types.CompareString(buf0.GetString(i), buf1.GetString(i), b.collation)
{{- else if eq .type.ETName "Duration" }}
		val := cmp.Compare(arg0[i], arg1[i])
{{- else if eq .type.ETName "Datetime" }}
		val := arg0[i].Compare(arg1[i])
{{- else if eq .type.ETName "Decimal" }}
		val := arg0[i].Compare(&arg1[i])
{{- end }}
		i64s[i] = boolToInt64(val {{ .compare.Operator }} 0)
	}
	return nil
}

func (b *builtin{{ .compare.CompareName }}{{ .type.TypeName }}Sig) vectorized() bool {
	return true
}
`))

var builtinNullEQCompareVecTpl = template.Must(template.New("").Parse(`
func (b *builtin{{ .compare.CompareName }}{{ .type.TypeName }}Sig) vecEvalInt(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	buf0, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(buf0)
	if err := b.args[0].VecEval{{ .type.TypeName }}(ctx, input, buf0); err != nil {
		return err
	}
	buf1, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(buf1)
	if err := b.args[1].VecEval{{ .type.TypeName }}(ctx, input, buf1); err != nil {
		return err
	}

{{ if .type.Fixed }}
	arg0 := buf0.{{ .type.TypeNameInColumn }}s()
	arg1 := buf1.{{ .type.TypeNameInColumn }}s()
{{- end }}
	result.ResizeInt64(n, false)
	i64s := result.Int64s()
	for i := 0; i < n; i++ {
		isNull0 := buf0.IsNull(i)
		isNull1 := buf1.IsNull(i)
		switch {
		case isNull0 && isNull1:
			i64s[i] = 1
		case isNull0 != isNull1:
			i64s[i] = 0
{{- if eq .type.ETName "Json" }}
		case types.CompareBinaryJSON(buf0.GetJSON(i), buf1.GetJSON(i)) == 0:
{{- else if eq .type.ETName "Real" }}
		case cmp.Compare(arg0[i], arg1[i]) == 0:
{{- else if eq .type.ETName "String" }}
		case types.CompareString(buf0.GetString(i), buf1.GetString(i), b.collation) == 0:
{{- else if eq .type.ETName "Duration" }}
		case cmp.Compare(arg0[i], arg1[i]) == 0:
{{- else if eq .type.ETName "Datetime" }}
		case arg0[i].Compare(arg1[i]) == 0:
{{- else if eq .type.ETName "Decimal" }}
		case arg0[i].Compare(&arg1[i]) == 0:
{{- end }}
			i64s[i] = 1
		}
	}
	return nil
}

func (b *builtin{{ .compare.CompareName }}{{ .type.TypeName }}Sig) vectorized() bool {
	return true
}
`))

var builtinCoalesceCompareVecTpl = template.Must(template.New("").Parse(`
// NOTE: Coalesce just return the first non-null item, but vectorization do each item, which would incur additional errors. If this case happen,
// the vectorization falls back to the scalar execution.
func (b *builtin{{ .compare.CompareName }}{{ .type.TypeName }}Sig) fallbackEval{{ .type.TypeName }}(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	{{ if .type.Fixed }}
	x := result.{{ .type.TypeNameInColumn }}s()
	for i := 0; i < n; i++ {
		res, isNull, err := b.eval{{ .type.TypeName }}(ctx, input.GetRow(i))
		if err != nil {
			return err
		}
		result.SetNull(i, isNull)
		if isNull {
			continue
		}
		{{ if eq .type.TypeName "Decimal" }}
			x[i] = *res
		{{ else if eq .type.TypeName "Duration" }}
			x[i] = res.Duration
		{{ else }}
			x[i] = res
		{{ end }}
	}
	{{ else }}
	result.Reserve{{ .type.TypeNameInColumn }}(n)
	for i := 0; i < n; i++ {
		res, isNull, err := b.eval{{ .type.TypeName }}(ctx, input.GetRow(i))
		if err != nil {
			return err
		}
		if isNull {
			result.AppendNull()
			continue
		}
		result.Append{{ .type.TypeNameInColumn }}(res)
	}
	{{ end -}}
	return nil
}

{{ if .type.Fixed }}
func (b *builtin{{ .compare.CompareName }}{{ .type.TypeName }}Sig) vecEval{{ .type.TypeName }}(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	result.Resize{{ .type.TypeNameInColumn }}(n, true)
	i64s := result.{{ .type.TypeNameInColumn }}s()
	buf1, err := b.bufAllocator.get()
	if err != nil {
		return err
	}
	defer b.bufAllocator.put(buf1)
	beforeWarns := warningCount(ctx)
	for j := 0; j < len(b.args); j++{
		err := b.args[j].VecEval{{ .type.TypeName }}(ctx, input, buf1)
        {{- if eq .type.TypeName "Time" }}
        fsp := b.tp.GetDecimal()
        {{- end }}
		afterWarns := warningCount(ctx)
		if err != nil || afterWarns > beforeWarns {
			if afterWarns > beforeWarns {
				truncateWarnings(ctx, beforeWarns)
			}
			return b.fallbackEval{{ .type.TypeName }}(ctx, input, result)
		}
		args := buf1.{{ .type.TypeNameInColumn }}s()
		for i := 0; i < n; i++ {
			if !buf1.IsNull(i) && result.IsNull(i) {
				i64s[i] = args[i]
                {{- if eq .type.TypeName "Time" }}
                i64s[i].SetFsp(fsp)
                {{- end }}
				result.SetNull(i, false)
			}
		}
	}
	return nil
}
{{ else }}
func (b *builtin{{ .compare.CompareName }}{{ .type.TypeName }}Sig) vecEval{{ .type.TypeName }}(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	argLen := len(b.args)

	bufs := make([]*chunk.Column, argLen)
	beforeWarns := warningCount(ctx)
	for i := 0; i < argLen; i++ {
		buf, err := b.bufAllocator.get()
		if err != nil {
			return err
		}
		defer b.bufAllocator.put(buf)
		err = b.args[i].VecEval{{ .type.TypeName }}(ctx, input, buf)
		afterWarns := warningCount(ctx)
		if err != nil || afterWarns > beforeWarns {
			if afterWarns > beforeWarns {
				truncateWarnings(ctx, beforeWarns)
			}
			return b.fallbackEval{{ .type.TypeName }}(ctx, input, result)
		}
		bufs[i]=buf
	}
	result.Reserve{{ .type.TypeName }}(n)

	for i := 0; i < n; i++ {
		for j := 0; j < argLen; j++ {
			if !bufs[j].IsNull(i) {
				result.Append{{ .type.TypeName }}(bufs[j].Get{{ .type.TypeName }}(i))
				break
			}
			if j == argLen-1 && bufs[j].IsNull(i) {
				result.AppendNull()
			}
		}
	}
	return nil
}


{{ end }}

func (b *builtin{{ .compare.CompareName }}{{ .type.TypeName }}Sig) vectorized() bool {
	return true
}

`))

const builtinCompareVecTestHeader = `import (
	"testing"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/types"
)

var vecGeneratedBuiltinCompareCases = map[string][]vecExprBenchCase{
`

var builtinCompareVecTestFuncHeader = template.Must(template.New("").Parse(`	ast.{{ .CompareName }}: {
`))

var builtinCompareVecTestCase = template.Must(template.New("").Parse(`		{retEvalType: types.ETInt, childrenTypes: []types.EvalType{types.ET{{ .ETName }}, types.ET{{ .ETName }}}},
`))

var builtinCompareVecTestFuncTail = `	},
`

var builtinCompareVecTestTail = `}

func TestVectorizedGeneratedBuiltinCompareEvalOneVec(t *testing.T) {
	testVectorizedEvalOneVec(t, vecGeneratedBuiltinCompareCases)
}

func TestVectorizedGeneratedBuiltinCompareFunc(t *testing.T) {
	testVectorizedBuiltinFunc(t, vecGeneratedBuiltinCompareCases)
}

func BenchmarkVectorizedGeneratedBuiltinCompareEvalOneVec(b *testing.B) {
	benchmarkVectorizedEvalOneVec(b, vecGeneratedBuiltinCompareCases)
}

func BenchmarkVectorizedGeneratedBuiltinCompareFunc(b *testing.B) {
	benchmarkVectorizedBuiltinFunc(b, vecGeneratedBuiltinCompareCases)
}
`

var builtinCoalesceCompareVecTestFunc = template.Must(template.New("").Parse(`
{
	retEvalType: types.ET{{ .ETName }},
	childrenTypes: []types.EvalType{types.ET{{ .ETName }}, types.ET{{ .ETName }}, types.ET{{ .ETName }}},
	geners: []dataGenerator{
		gener{*newDefaultGener(0.2, types.ET{{ .ETName }})},
		gener{*newDefaultGener(0.2, types.ET{{ .ETName }})},
		gener{*newDefaultGener(0.2, types.ET{{ .ETName }})},
	},
},


`))

type CompareContext struct {
	// Describe the name of CompareContext(LT/LE/GT/GE/EQ/NE/NullEQ)
	CompareName string
	// Compare Operators
	Operator string
}

var comparesMap = []CompareContext{
	{CompareName: "LT", Operator: "<"},
	{CompareName: "LE", Operator: "<="},
	{CompareName: "GT", Operator: ">"},
	{CompareName: "GE", Operator: ">="},
	{CompareName: "EQ", Operator: "=="},
	{CompareName: "NE", Operator: "!="},
	{CompareName: "NullEQ"},
	{CompareName: "Coalesce"},
}

var typesMap = []TypeContext{
	TypeInt,
	TypeReal,
	TypeDecimal,
	TypeString,
	TypeDatetime,
	TypeDuration,
	TypeJSON,
}

func generateDotGo(fileName string, compares []CompareContext, types []TypeContext) (err error) {
	w := new(bytes.Buffer)
	w.WriteString(header)
	w.WriteString(newLine)
	w.WriteString(builtinCompareImports)

	var ctx = make(map[string]interface{})
	for _, compareCtx := range compares {
		for _, typeCtx := range types {
			ctx["compare"] = compareCtx
			ctx["type"] = typeCtx
			if compareCtx.CompareName == "NullEQ" {
				if typeCtx.TypeName == TypeInt.TypeName {
					continue
				}
				err := builtinNullEQCompareVecTpl.Execute(w, ctx)
				if err != nil {
					return err
				}
			} else if compareCtx.CompareName == "Coalesce" {

				err := builtinCoalesceCompareVecTpl.Execute(w, ctx)
				if err != nil {
					return err
				}

			} else {
				if typeCtx.TypeName == TypeInt.TypeName {
					continue
				}
				err := builtinCompareVecTpl.Execute(w, ctx)
				if err != nil {
					return err
				}
			}
		}
	}
	data, err := format.Source(w.Bytes())
	if err != nil {
		log.Println("[Warn]", fileName+": gofmt failed", err)
		data = w.Bytes() // write original data for debugging
	}
	return os.WriteFile(fileName, data, 0644)
}

func generateTestDotGo(fileName string, compares []CompareContext, types []TypeContext) error {
	w := new(bytes.Buffer)
	w.WriteString(header)
	w.WriteString(builtinCompareVecTestHeader)

	for _, compareCtx := range compares {
		if compareCtx.CompareName == "Coalesce" {
			err := builtinCompareVecTestFuncHeader.Execute(w, CompareContext{CompareName: "Coalesce"})
			if err != nil {
				return err
			}
			for _, typeCtx := range types {
				err := builtinCoalesceCompareVecTestFunc.Execute(w, typeCtx)
				if err != nil {
					return err
				}
			}
			w.WriteString(builtinCompareVecTestFuncTail)
			continue
		}
		err := builtinCompareVecTestFuncHeader.Execute(w, compareCtx)
		if err != nil {
			return err
		}
		for _, typeCtx := range types {
			if typeCtx.TypeName == TypeInt.TypeName {
				continue
			}
			err := builtinCompareVecTestCase.Execute(w, typeCtx)
			if err != nil {
				return err
			}
		}
		w.WriteString(builtinCompareVecTestFuncTail)
	}
	w.WriteString(builtinCompareVecTestTail)

	data, err := format.Source(w.Bytes())
	if err != nil {
		log.Println("[Warn]", fileName+": gofmt failed", err)
		data = w.Bytes() // write original data for debugging
	}
	return os.WriteFile(fileName, data, 0644)
}

// generateOneFile generate one xxx.go file and the associated xxx_test.go file.
func generateOneFile(fileNamePrefix string, compares []CompareContext,
	types []TypeContext) (err error) {

	err = generateDotGo(fileNamePrefix+".go", compares, types)
	if err != nil {
		return
	}
	err = generateTestDotGo(fileNamePrefix+"_test.go", compares, types)
	return
}

func main() {
	flag.Parse()
	var err error
	outputDir := "."
	err = generateOneFile(filepath.Join(outputDir, "builtin_compare_vec_generated"),
		comparesMap, typesMap)
	if err != nil {
		log.Fatalln("generateOneFile", err)
	}
}
