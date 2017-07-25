package query

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"sort"

	"github.com/influxdata/influxdb/influxql"
)

type CompileOptions struct {
	Now time.Time
}

type compiledStatement struct {
	// Sources holds the data sources this will query from.
	Sources influxql.Sources

	// Dimensions holds the groupings for the statement.
	Dimensions []string

	// Tags holds all of the necessary tags used in this statement.
	// These are the tags that will be selected from the storage engine.
	Tags map[string]struct{}

	// Condition is the condition used for accessing data.
	Condition influxql.Expr

	// TimeRange is the TimeRange for selecting data.
	TimeRange TimeRange

	// Interval holds the time grouping interval.
	Interval influxql.Interval

	// Ascending is true if the time ordering is ascending.
	Ascending bool

	// FunctionCalls holds a reference to the read edge of all of the
	// function calls that have been instantiated.
	FunctionCalls []*ReadEdge

	// OnlySelectors is set to true when there are no aggregate functions.
	OnlySelectors bool

	// HasDistinct is set when the distinct() function is encountered.
	HasDistinct bool

	// FillOption contains the fill option for aggregates.
	FillOption influxql.FillOption

	// TopBottomFunction is set to top or bottom when one of those functions are
	// used in the statement.
	TopBottomFunction string

	// AuxiliaryFields holds a mapping to the auxiliary fields that need to be
	// selected. This maps the raw VarRef to a pointer to a shared VarRef. The
	// pointer is used for instantiating references to the shared variable so
	// type mapping gets shared.
	AuxiliaryFields *AuxiliaryFields

	// Fields holds all of the compiled fields that will be used.
	Fields []*compiledField

	// Options holds the configured compiler options.
	Options CompileOptions
}

type CompiledStatement interface {
	Select(plan *Plan) ([]*ReadEdge, []string, error)
}

func newCompiler(stmt *influxql.SelectStatement, opt CompileOptions) *compiledStatement {
	if opt.Now.IsZero() {
		opt.Now = time.Now().UTC()
	}
	return &compiledStatement{
		OnlySelectors: true,
		Fields:        make([]*compiledField, 0, len(stmt.Fields)),
		Tags:          make(map[string]struct{}),
		Ascending:     true,
		Options:       opt,
	}
}

// Wildcard represents a wildcard within a field.
type wildcard struct {
	// NameFilters are the regexp filters for selecting fields. If this is
	// nil, no fields are filtered because of their name.
	NameFilters []*regexp.Regexp

	// TypeFilters holds a list of all of the types forbidden to be used
	// because of a function.
	TypeFilters map[influxql.DataType]struct{}
}

// compiledField holds the compilation state for a field.
type compiledField struct {
	// This holds the global state from the compiled statement.
	global *compiledStatement

	// Field contains the original field associated with this field.
	Field *influxql.Field

	// Output contains the output edge for this field.
	Output *ReadEdge

	// Symbols contains the symbol table for this field.
	Symbols *SymbolTable

	// Wildcard contains the wildcard expression to be used when resolving
	// wildcards.
	Wildcard *wildcard

	// WildcardRef holds the resolved wildcard reference. Each field can
	// only have a wildcard resolve to a single field or tag.
	// This should not be set at the same time as Wildcard.
	WildcardRef *influxql.VarRef
}

// compileExpr creates the node that executes the expression and connects that
// node to the WriteEdge as the output.
func (c *compiledField) compileExpr(expr influxql.Expr, out *WriteEdge) error {
	switch expr := expr.(type) {
	case *influxql.VarRef:
		// A bare variable reference will require auxiliary fields.
		c.global.requireAuxiliaryFields()
		// Add a symbol that resolves to this write edge.
		c.Symbols.Table[out] = &AuxiliarySymbol{Ref: expr}
		return nil
	case *influxql.Wildcard:
		// Wildcards use auxiliary fields. We assume there will be at least one
		// expansion.
		c.global.requireAuxiliaryFields()
		c.wildcard()
		c.Symbols.Table[out] = &AuxiliaryWildcardSymbol{}
		return nil
	case *influxql.RegexLiteral:
		c.global.requireAuxiliaryFields()
		c.wildcardFilter(expr.Val)
		c.Symbols.Table[out] = &AuxiliaryWildcardSymbol{}
		return nil
	case *influxql.Call:
		switch expr.Name {
		case "percentile":
			return c.compilePercentile(expr.Args, out)
		case "sample":
			return c.compileSample(expr.Args, out)
		case "distinct":
			return c.compileDistinct(expr.Args, out, false)
		case "top", "bottom":
			return c.compileTopBottom(expr, out)
		case "derivative", "non_negative_derivative":
			isNonNegative := expr.Name == "non_negative_derivative"
			return c.compileDerivative(expr.Args, isNonNegative, out)
		case "difference", "non_negative_difference":
			isNonNegative := expr.Name == "non_negative_difference"
			return c.compileDifference(expr.Args, isNonNegative, out)
		case "cumulative_sum":
			return c.compileCumulativeSum(expr.Args, out)
		case "moving_average":
			return c.compileMovingAverage(expr.Args, out)
		case "integral":
			return c.compileIntegral(expr.Args, out)
		case "holt_winters", "holt_winters_with_fit":
			withFit := expr.Name == "holt_winters_with_fit"
			return c.compileHoltWinters(expr.Args, withFit, out)
		default:
			return c.compileFunction(expr, out)
		}
	case *influxql.Distinct:
		call := expr.NewCall()
		return c.compileDistinct(call.Args, out, false)
	case *influxql.BinaryExpr:
		// Check if either side is a literal so we only compile one side if it is.
		if _, ok := expr.LHS.(influxql.Literal); ok {
		} else if _, ok := expr.RHS.(influxql.Literal); ok {
		} else {
			// Construct a binary expression and an input edge for each side.
			node := &BinaryExpr{Op: expr.Op, Output: out}
			out.Node = node

			// Process the left side.
			var lhs *WriteEdge
			lhs, node.LHS = AddEdge(nil, node)
			if err := c.compileExpr(expr.LHS, lhs); err != nil {
				return err
			}

			// Process the right side.
			var rhs *WriteEdge
			rhs, node.RHS = AddEdge(nil, node)
			if err := c.compileExpr(expr.RHS, rhs); err != nil {
				return err
			}
			return nil
		}
	case *influxql.ParenExpr:
		return c.compileExpr(expr.Expr, out)
	}
	return errors.New("unimplemented")
}

func (c *compiledField) compileSymbol(name string, field influxql.Expr, out *WriteEdge) error {
	// Must be a variable reference, wildcard, or regexp.
	switch arg0 := field.(type) {
	case *influxql.VarRef:
		return c.compileVarRef(arg0, out)
	case *influxql.Wildcard:
		c.wildcardFunction(name)
		c.Symbols.Table[out] = &WildcardSymbol{}
		return nil
	case *influxql.RegexLiteral:
		c.wildcardFunctionFilter(name, arg0.Val)
		c.Symbols.Table[out] = &WildcardSymbol{}
		return nil
	default:
		return fmt.Errorf("expected field argument in %s()", name)
	}
}

func (c *compiledField) compileFunction(expr *influxql.Call, out *WriteEdge) error {
	// Create the function call and send its output to the write edge.
	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	switch expr.Name {
	case "count", "min", "max", "sum", "first", "last", "mean":
		call := &FunctionCall{
			Name:       expr.Name,
			Dimensions: c.global.Dimensions,
			Interval:   c.global.Interval,
			TimeRange:  c.global.TimeRange,
			Output:     out,
		}
		out.Node = call
		out, call.Input = AddEdge(nil, call)
	case "median":
		median := &Median{Output: out}
		out.Node = median
		out, median.Input = AddEdge(nil, median)
	case "mode":
		mode := &Mode{Output: out}
		out.Node = mode
		out, mode.Input = AddEdge(nil, mode)
	case "stddev":
		stddev := &Stddev{Output: out}
		out.Node = stddev
		out, stddev.Input = AddEdge(nil, stddev)
	case "spread":
		spread := &Spread{Output: out}
		out.Node = spread
		out, spread.Input = AddEdge(nil, spread)
	default:
		return fmt.Errorf("undefined function %s()", expr.Name)
	}

	if exp, got := 1, len(expr.Args); exp != got {
		return fmt.Errorf("invalid number of arguments for %s, expected %d, got %d", expr.Name, exp, got)
	}

	// Mark down some meta properties related to the function for query validation.
	switch expr.Name {
	case "max", "min", "first", "last":
		// top/bottom are not included here since they are not typical functions.
	default:
		c.global.OnlySelectors = false
	}

	// If this is a call to count(), allow distinct() to be used as the function argument.
	if expr.Name == "count" {
		// If we have count(), the argument may be a distinct() call.
		if arg0, ok := expr.Args[0].(*influxql.Call); ok && arg0.Name == "distinct" {
			return c.compileDistinct(arg0.Args, out, true)
		} else if arg0, ok := expr.Args[0].(*influxql.Distinct); ok {
			call := arg0.NewCall()
			return c.compileDistinct(call.Args, out, true)
		}
	}
	return c.compileSymbol(expr.Name, expr.Args[0], out)
}

func (c *compiledField) compilePercentile(args []influxql.Expr, out *WriteEdge) error {
	if exp, got := 2, len(args); got != exp {
		return fmt.Errorf("invalid number of arguments for percentile, expected %d, got %d", exp, got)
	}

	var percentile float64
	switch lit := args[1].(type) {
	case *influxql.IntegerLiteral:
		percentile = float64(lit.Val)
	case *influxql.NumberLiteral:
		percentile = lit.Val
	default:
		return fmt.Errorf("expected float argument in percentile()")
	}

	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	p := &Percentile{Number: percentile, Output: out}
	out.Node = p
	out, p.Input = AddEdge(nil, p)
	return c.compileSymbol("percentile", args[0], out)
}

func (c *compiledField) compileSample(args []influxql.Expr, out *WriteEdge) error {
	if exp, got := 2, len(args); got != exp {
		return fmt.Errorf("invalid number of arguments for sample, expected %d, got %d", exp, got)
	}

	var n int
	switch lit := args[1].(type) {
	case *influxql.IntegerLiteral:
		n = int(lit.Val)
	default:
		return fmt.Errorf("expected integer argument in sample()")
	}

	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	s := &Sample{N: n, Output: out}
	out.Node = s
	out, s.Input = AddEdge(nil, s)
	return c.compileSymbol("sample", args[0], out)
}

func (c *compiledField) compileDerivative(args []influxql.Expr, isNonNegative bool, out *WriteEdge) error {
	name := "derivative"
	if isNonNegative {
		name = "non_negative_derivative"
	}

	if min, max, got := 1, 2, len(args); got > max || got < min {
		return fmt.Errorf("invalid number of arguments for %s, expected at least %d but no more than %d, got %d", name, min, max, got)
	}

	// Retrieve the duration from the derivative() call, if specified.
	dur := time.Second
	if len(args) == 2 {
		switch arg1 := args[1].(type) {
		case *influxql.DurationLiteral:
			if arg1.Val <= 0 {
				return fmt.Errorf("duration argument must be positive, got %s", influxql.FormatDuration(arg1.Val))
			}
			dur = arg1.Val
		default:
			return fmt.Errorf("second argument to %s must be a duration, got %T", name, args[1])
		}
	} else if !c.global.Interval.IsZero() {
		// Otherwise use the group by interval, if specified.
		dur = c.global.Interval.Duration
	}

	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	d := &Derivative{
		Duration:      dur,
		IsNonNegative: isNonNegative,
		Output:        out,
	}
	out.Node = d
	out, d.Input = AddEdge(nil, d)
	c.global.OnlySelectors = false

	// Must be a variable reference, function, wildcard, or regexp.
	switch arg0 := args[0].(type) {
	case *influxql.Call:
		if c.global.Interval.IsZero() {
			return fmt.Errorf("%s aggregate requires a GROUP BY interval", name)
		}
		return c.compileExpr(arg0, out)
	default:
		if !c.global.Interval.IsZero() {
			return fmt.Errorf("aggregate function required inside the call to %s", name)
		}
		return c.compileSymbol(name, arg0, out)
	}
}

func (c *compiledField) compileElapsed(args []influxql.Expr, out *WriteEdge) error {
	if min, max, got := 1, 2, len(args); got > max || got < min {
		return fmt.Errorf("invalid number of arguments for elapsed, expected at least %d but no more than %d, got %d", min, max, got)
	}

	// Retrieve the duration from the elapsed() call, if specified.
	dur := time.Nanosecond
	if len(args) == 2 {
		switch arg1 := args[1].(type) {
		case *influxql.DurationLiteral:
			if arg1.Val <= 0 {
				return fmt.Errorf("duration argument must be positive, got %s", influxql.FormatDuration(arg1.Val))
			}
			dur = arg1.Val
		default:
			return fmt.Errorf("second argument to elapsed must be a duration, got %T", args[1])
		}
	}

	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	e := &Elapsed{
		Duration: dur,
		Output:   out,
	}
	out.Node = e
	out, e.Input = AddEdge(nil, e)
	c.global.OnlySelectors = false

	// Must be a variable reference, function, wildcard, or regexp.
	switch arg0 := args[0].(type) {
	case *influxql.Call:
		if c.global.Interval.IsZero() {
			return fmt.Errorf("%s aggregate requires a GROUP BY interval", arg0.Name)
		}
		return c.compileExpr(arg0, out)
	default:
		if !c.global.Interval.IsZero() {
			return fmt.Errorf("aggregate function required inside the call to elapsed")
		}
		return c.compileSymbol("elapsed", arg0, out)
	}
}

func (c *compiledField) compileDifference(args []influxql.Expr, isNonNegative bool, out *WriteEdge) error {
	name := "difference"
	if isNonNegative {
		name = "non_negative_difference"
	}

	if got := len(args); got != 1 {
		return fmt.Errorf("invalid number of arguments for %s, expected 1, got %d", name, got)
	}

	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	d := &Difference{
		IsNonNegative: isNonNegative,
		Output:        out,
	}
	out.Node = d
	out, d.Input = AddEdge(nil, d)
	c.global.OnlySelectors = false

	// Must be a variable reference, function, wildcard, or regexp.
	switch arg0 := args[0].(type) {
	case *influxql.Call:
		if c.global.Interval.IsZero() {
			return fmt.Errorf("%s aggregate requires a GROUP BY interval", name)
		}
		return c.compileExpr(arg0, out)
	default:
		if !c.global.Interval.IsZero() {
			return fmt.Errorf("aggregate function required inside the call to %s", name)
		}
		return c.compileSymbol(name, arg0, out)
	}
}

func (c *compiledField) compileCumulativeSum(args []influxql.Expr, out *WriteEdge) error {
	if got := len(args); got != 1 {
		return fmt.Errorf("invalid number of arguments for cumulative_sum, expected 1, got %d", got)
	}

	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	cs := &CumulativeSum{Output: out}
	out.Node = cs
	out, cs.Input = AddEdge(nil, cs)
	c.global.OnlySelectors = false

	// Must be a variable reference, function, wildcard, or regexp.
	switch arg0 := args[0].(type) {
	case *influxql.Call:
		if c.global.Interval.IsZero() {
			return fmt.Errorf("cumulative_sum aggregate requires a GROUP BY interval")
		}
		return c.compileExpr(arg0, out)
	default:
		if !c.global.Interval.IsZero() {
			return fmt.Errorf("aggregate function required inside the call to cumulative_sum")
		}
		return c.compileSymbol("cumulative_sum", arg0, out)
	}
}

func (c *compiledField) compileMovingAverage(args []influxql.Expr, out *WriteEdge) error {
	if got := len(args); got != 2 {
		return fmt.Errorf("invalid number of arguments for moving_average, expected 2, got %d", got)
	}

	var windowSize int
	switch arg1 := args[1].(type) {
	case *influxql.IntegerLiteral:
		if arg1.Val <= 1 {
			return fmt.Errorf("moving_average window must be greater than 1, got %d", arg1.Val)
		} else if int64(int(arg1.Val)) != arg1.Val {
			return fmt.Errorf("moving_average window too large, got %d", arg1.Val)
		}
		windowSize = int(arg1.Val)
	default:
		return fmt.Errorf("second argument for moving_average must be an integer, got %T", args[1])
	}

	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	m := &MovingAverage{
		WindowSize: windowSize,
		Output:     out,
	}
	out.Node = m
	out, m.Input = AddEdge(nil, m)
	c.global.OnlySelectors = false

	// Must be a variable reference, function, wildcard, or regexp.
	switch arg0 := args[0].(type) {
	case *influxql.Call:
		if c.global.Interval.IsZero() {
			return fmt.Errorf("moving_average aggregate requires a GROUP BY interval")
		}
		return c.compileExpr(arg0, out)
	default:
		if !c.global.Interval.IsZero() {
			return fmt.Errorf("aggregate function required inside the call to moving_average")
		}
		return c.compileSymbol("moving_average", arg0, out)
	}
}

func (c *compiledField) compileIntegral(args []influxql.Expr, out *WriteEdge) error {
	if min, max, got := 1, 2, len(args); got > max || got < min {
		return fmt.Errorf("invalid number of arguments for integral, expected at least %d but no more than %d, got %d", min, max, got)
	}

	dur := time.Second
	if len(args) == 2 {
		switch arg1 := args[1].(type) {
		case *influxql.DurationLiteral:
			if arg1.Val <= 0 {
				return fmt.Errorf("duration argument must be positive, got %s", influxql.FormatDuration(arg1.Val))
			}
			dur = arg1.Val
		default:
			return errors.New("second argument must be a duration")
		}
	}

	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	i := &Integral{
		Duration: dur,
		Output:   out,
	}
	out.Node = i
	out, i.Input = AddEdge(nil, i)
	c.global.OnlySelectors = false

	// Must be a variable reference, wildcard, or regexp.
	return c.compileSymbol("integral", args[0], out)
}

func (c *compiledField) compileHoltWinters(args []influxql.Expr, withFit bool, out *WriteEdge) error {
	name := "holt_winters"
	if withFit {
		name = "holt_winters_with_fit"
	}

	if exp, got := 3, len(args); got != exp {
		return fmt.Errorf("invalid number of arguments for %s, expected %d, got %d", name, exp, got)
	}

	n, ok := args[1].(*influxql.IntegerLiteral)
	if !ok {
		return fmt.Errorf("expected integer argument as second arg in %s", name)
	} else if n.Val <= 0 {
		return fmt.Errorf("second arg to %s must be greater than 0, got %d", name, n.Val)
	}

	s, ok := args[2].(*influxql.IntegerLiteral)
	if !ok {
		return fmt.Errorf("expected integer argument as third arg in %s", name)
	} else if s.Val < 0 {
		return fmt.Errorf("third arg to %s cannot be negative, got %d", name, s.Val)
	}

	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	hw := &HoltWinters{
		N:       int(n.Val),
		S:       int(s.Val),
		WithFit: withFit,
		Output:  out,
	}
	out.Node = hw
	out, hw.Input = AddEdge(nil, hw)
	c.global.OnlySelectors = false

	call, ok := args[0].(*influxql.Call)
	if !ok {
		return fmt.Errorf("must use aggregate function with %s", name)
	} else if c.global.Interval.IsZero() {
		return fmt.Errorf("%s aggregate requires a GROUP BY interval", name)
	}
	return c.compileExpr(call, out)
}

func (c *compiledStatement) linkAuxiliaryFields() {
	if c.AuxiliaryFields == nil {
		return
	}

	if len(c.FunctionCalls) == 0 {
		// Create a default IteratorCreator for this AuxiliaryFields.
		var out *WriteEdge
		out, c.AuxiliaryFields.Input = AddEdge(nil, c.AuxiliaryFields)

		merge := &Merge{Output: out}
		for _, source := range c.Sources {
			switch source := source.(type) {
			case *influxql.Measurement:
				ic := &IteratorCreator{
					Dimensions:      c.Dimensions,
					Tags:            c.Tags,
					TimeRange:       c.TimeRange,
					AuxiliaryFields: c.AuxiliaryFields,
					Measurement:     source,
				}
				ic.Output = merge.AddInput(ic)
			default:
				panic("unimplemented")
			}
		}
		out.Node = merge
	} else {
		c.AuxiliaryFields.Input, c.AuxiliaryFields.Output = c.FunctionCalls[0].Insert(c.AuxiliaryFields)
	}
}

func (c *compiledField) compileDistinct(args []influxql.Expr, out *WriteEdge, nested bool) error {
	if len(args) == 0 {
		return errors.New("distinct function requires at least one argument")
	} else if len(args) != 1 {
		return errors.New("distinct function can only have one argument")
	}

	arg0, ok := args[0].(*influxql.VarRef)
	if !ok {
		return errors.New("expected field argument in distinct()")
	}

	// Add the distinct node to the graph.
	d := &Distinct{Output: out}
	if !nested {
		// Add as a function call if this is not nested.
		c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	}
	out.Node = d
	out, d.Input = AddEdge(nil, d)
	c.global.HasDistinct = true

	// Add the variable reference to the graph to complete the graph.
	return c.compileVarRef(arg0, out)
}

func (c *compiledField) compileTopBottom(call *influxql.Call, out *WriteEdge) error {
	if c.global.TopBottomFunction != "" {
		return fmt.Errorf("selector function %s() cannot be combined with other functions", c.global.TopBottomFunction)
	}

	if exp, got := 2, len(call.Args); got < exp {
		return fmt.Errorf("invalid number of arguments for %s, expected at least %d, got %d", call.Name, exp, got)
	}

	limit, ok := call.Args[len(call.Args)-1].(*influxql.IntegerLiteral)
	if !ok {
		return fmt.Errorf("expected integer as last argument in %s(), found %s", call.Name, call.Args[len(call.Args)-1])
	} else if limit.Val <= 0 {
		return fmt.Errorf("limit (%d) in %s function must be at least 1", limit.Val, call.Name)
	}

	ref, ok := call.Args[0].(*influxql.VarRef)
	if !ok {
		return fmt.Errorf("expected first argument to be a field in %s(), found %s", call.Name, call.Args[0])
	}

	var dimensions []influxql.VarRef
	if len(call.Args) > 2 {
		dimensions = make([]influxql.VarRef, 0, len(call.Args))
		for _, v := range call.Args[1 : len(call.Args)-1] {
			ref, ok := v.(*influxql.VarRef)
			if ok {
				dimensions = append(dimensions, *ref)
			} else {
				return fmt.Errorf("only fields or tags are allowed in %s(), found %s", call.Name, v)
			}

			// Add a field for each of the listed dimensions.
			in, out := NewEdge(nil)
			field := &compiledField{
				global: c.global,
				Field:  &influxql.Field{Expr: ref},
				Output: out,
				Symbols: &SymbolTable{
					Table: make(map[*WriteEdge]Symbol),
				},
			}
			c.global.Fields = append(c.global.Fields, field)
			if err := field.compileExpr(ref, in); err != nil {
				return err
			}
		}
	}
	c.global.TopBottomFunction = call.Name

	selector := &TopBottomSelector{
		Name:      call.Name,
		Limit:     int(limit.Val),
		TimeRange: c.global.TimeRange,
		Output:    out,
	}
	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	out.Node = selector
	out, selector.Input = AddEdge(nil, selector)

	// If we are grouping by some dimension, create a min/max call iterator
	// with those dimensions.
	if len(dimensions) > 0 {
		fcall := &FunctionCall{
			Dimensions: c.global.Dimensions,
			Interval:   c.global.Interval,
			TimeRange:  c.global.TimeRange,
			Output:     out,
		}
		out.Node = fcall

		if call.Name == "top" {
			fcall.Name = "max"
		} else {
			fcall.Name = "min"
		}
		fcall.GroupBy = make(map[string]struct{}, len(dimensions))
		for _, d := range dimensions {
			c.global.Tags[d.Val] = struct{}{}
			fcall.GroupBy[d.Val] = struct{}{}
		}
		for _, d := range c.global.Dimensions {
			fcall.GroupBy[d] = struct{}{}
		}

		out, fcall.Input = AddEdge(nil, fcall)
	}
	return c.compileVarRef(ref, out)
}

func (c *compiledField) wildcard() {
	if c.Wildcard == nil {
		c.Wildcard = &wildcard{
			TypeFilters: make(map[influxql.DataType]struct{}),
		}
	}
}

func (c *compiledField) wildcardFilter(filter *regexp.Regexp) {
	c.wildcard()
	c.Wildcard.NameFilters = append(c.Wildcard.NameFilters, filter)
}

func (c *compiledField) wildcardFunction(name string) {
	c.wildcard()
	switch name {
	default:
		c.Wildcard.TypeFilters[influxql.String] = struct{}{}
	case "count", "first", "last", "distinct", "elapsed", "mode", "sample":
		c.Wildcard.TypeFilters[influxql.Boolean] = struct{}{}
	case "min", "max":
		// No restrictions.
	}

	// Do not allow selectors when we are using a wildcard.
	// While it is physically possible to only choose one value,
	// it is better to just pre-emptively stop this uncommon situation
	// from happening to begin with.
	c.global.OnlySelectors = false
}

func (c *compiledField) resolveSymbols(m ShardGroup) {
	for out, symbol := range c.Symbols.Table {
		symbol.resolve(m, c, out)
	}
}

func (c *compiledField) wildcardFunctionFilter(name string, filter *regexp.Regexp) {
	c.wildcardFunction(name)
	c.Wildcard.NameFilters = append(c.Wildcard.NameFilters, filter)
}

func (c *compiledField) compileVarRef(ref *influxql.VarRef, out *WriteEdge) error {
	c.Symbols.Table[out] = &VarRefSymbol{Ref: ref}
	return nil
}

// validateFields validates that the fields are mutually compatible with each other.
// This runs at the end of compilation but before linking.
func (c *compiledStatement) validateFields() error {
	// Validate that at least one field has been selected.
	if len(c.Fields) == 0 {
		return errors.New("at least 1 non-time field must be queried")
	}
	// Ensure there are not multiple calls if top/bottom is present.
	if len(c.FunctionCalls) > 1 && c.TopBottomFunction != "" {
		return fmt.Errorf("selector function %s() cannot be combined with other functions", c.TopBottomFunction)
	} else if len(c.FunctionCalls) == 0 {
		switch c.FillOption {
		case influxql.NoFill:
			return errors.New("fill(none) must be used with a function")
		case influxql.LinearFill:
			return errors.New("fill(linear) must be used with a function")
		}
		if !c.Interval.IsZero() {
			return errors.New("GROUP BY requires at least one aggregate function")
		}
	}
	// If a distinct() call is present, ensure there is exactly one function.
	if c.HasDistinct && (len(c.FunctionCalls) != 1 || c.AuxiliaryFields != nil) {
		return errors.New("aggregate function distinct() cannot be combined with other functions or fields")
	}
	// Validate we are using a selector or raw query if auxiliary fields are required.
	if c.AuxiliaryFields != nil {
		if !c.OnlySelectors {
			return fmt.Errorf("mixing aggregate and non-aggregate queries is not supported")
		} else if len(c.FunctionCalls) > 1 {
			return fmt.Errorf("mixing multiple selector functions with tags or fields is not supported")
		}
	}
	return nil
}

// validateDimensions validates that the dimensions are appropriate for this type of query.
func (c *compiledStatement) validateDimensions() error {
	if !c.Interval.IsZero() {
		// There must be a lower limit that wasn't implicitly set.
		if c.TimeRange.Min.UnixNano() == influxql.MinTime {
			return errors.New("aggregate functions with GROUP BY time require a WHERE time clause with a lower limit")
		}
	}
	return nil
}

func Compile(stmt *influxql.SelectStatement, opt CompileOptions) (CompiledStatement, error) {
	// Compile each of the expressions.
	c := newCompiler(stmt, opt)
	c.Sources = append(c.Sources, stmt.Sources...)
	c.FillOption = stmt.Fill
	c.Ascending = stmt.TimeAscending()

	// Retrieve the condition expression and the time range.
	valuer := influxql.NowValuer{Now: c.Options.Now}
	if cond, timeRange, err := ParseCondition(stmt.Condition, &valuer); err != nil {
		return nil, err
	} else {
		c.Condition = cond
		if timeRange != nil {
			c.TimeRange = *timeRange
		}
	}

	// Read the dimensions of the query and retrieve the interval if it exists.
	if err := c.compileDimensions(stmt); err != nil {
		return nil, err
	}

	// Resolve the min and max times now that we know if there is an interval or not.
	if c.TimeRange.Min.IsZero() {
		c.TimeRange.Min = time.Unix(0, influxql.MinTime).UTC()
	}
	if c.TimeRange.Max.IsZero() {
		// If the interval is non-zero, then we have an aggregate query and
		// need to limit the maximum time to now() for backwards compatibility
		// and usability.
		if !c.Interval.IsZero() {
			c.TimeRange.Max = c.Options.Now
		} else {
			c.TimeRange.Max = time.Unix(0, influxql.MaxTime).UTC()
		}
	}

	for _, f := range stmt.Fields {
		if ref, ok := f.Expr.(*influxql.VarRef); ok && ref.Val == "time" {
			continue
		}

		in, out := NewEdge(nil)
		field := &compiledField{
			global: c,
			Field:  f,
			Output: out,
			Symbols: &SymbolTable{
				Table: make(map[*WriteEdge]Symbol),
			},
		}
		c.Fields = append(c.Fields, field)
		if err := field.compileExpr(f.Expr, in); err != nil {
			return nil, err
		}
	}

	if err := c.validateFields(); err != nil {
		return nil, err
	}
	if err := c.validateDimensions(); err != nil {
		return nil, err
	}
	return c, nil
}

// compileDimensions parses the dimensions and interval information from the
// SelectStatement. This sets the Dimensions and Interval for the compiledStatement
// and returns an error if it failed for some reason.
func (c *compiledStatement) compileDimensions(stmt *influxql.SelectStatement) error {
	c.Dimensions = make([]string, 0, len(stmt.Dimensions))
	for _, d := range stmt.Dimensions {
		switch expr := d.Expr.(type) {
		case *influxql.VarRef:
			if strings.ToLower(expr.Val) == "time" {
				return errors.New("time() is a function and expects at least one argument")
			}
			c.Dimensions = append(c.Dimensions, expr.Val)
		case *influxql.Call:
			// Ensure the call is time() and it has one or two duration arguments.
			// If we already have a duration
			if expr.Name != "time" {
				return errors.New("only time() calls allowed in dimensions")
			} else if got := len(expr.Args); got < 1 || got > 2 {
				return errors.New("time dimension expected 1 or 2 arguments")
			} else if lit, ok := expr.Args[0].(*influxql.DurationLiteral); !ok {
				return errors.New("time dimension must have duration argument")
			} else if c.Interval.Duration != 0 {
				return errors.New("multiple time dimensions not allowed")
			} else {
				c.Interval.Duration = lit.Val
				if len(expr.Args) == 2 {
					switch lit := expr.Args[1].(type) {
					case *influxql.DurationLiteral:
						c.Interval.Offset = lit.Val % c.Interval.Duration
					case *influxql.TimeLiteral:
						c.Interval.Offset = lit.Val.Sub(lit.Val.Truncate(c.Interval.Duration))
					case *influxql.Call:
						if lit.Name != "now" {
							return errors.New("time dimension offset function must be now()")
						} else if len(lit.Args) != 0 {
							return errors.New("time dimension offset now() function requires no arguments")
						}
						now := c.Options.Now
						c.Interval.Offset = now.Sub(now.Truncate(c.Interval.Duration))
					default:
						return errors.New("time dimension offset must be duration or now()")
					}
				}
			}
		case *influxql.Wildcard:
			return errors.New("unimplemented")
		case *influxql.RegexLiteral:
			return errors.New("unimplemented")
		default:
			return errors.New("only time and tag dimensions allowed")
		}
	}
	return nil
}

// TimeRange represents a range of time from Min to Max. The times are inclusive.
type TimeRange struct {
	Min, Max time.Time
}

// Intersect joins this TimeRange with another TimeRange. If one of the two is nil,
// this returns the non-nil one. If both are non-nil, the caller is modified and
// returned.
func (t *TimeRange) Intersect(other *TimeRange) *TimeRange {
	if other == nil {
		return t
	} else if t == nil {
		return other
	}

	if !other.Min.IsZero() {
		if t.Min.IsZero() || other.Min.After(t.Min) {
			t.Min = other.Min
		}
	}
	if !other.Max.IsZero() {
		if t.Max.IsZero() || other.Max.Before(t.Max) {
			t.Max = other.Max
		}
	}
	return t
}

// ParseCondition extracts the time range and the condition from an expression.
// We only support simple time ranges that are constrained with AND and are not nested.
// This throws an error when we encounter a time condition that is combined with OR
// to prevent returning unexpected results that we do not support.
func ParseCondition(cond influxql.Expr, valuer influxql.Valuer) (influxql.Expr, *TimeRange, error) {
	if cond == nil {
		return nil, nil, nil
	}

	switch cond := cond.(type) {
	case *influxql.BinaryExpr:
		if cond.Op == influxql.AND || cond.Op == influxql.OR {
			lhsExpr, lhsTime, err := ParseCondition(cond.LHS, valuer)
			if err != nil {
				return nil, nil, err
			}

			rhsExpr, rhsTime, err := ParseCondition(cond.RHS, valuer)
			if err != nil {
				return nil, nil, err
			}

			// If either of the two expressions has a time range and we are combining
			// them with OR, return an error since this isn't allowed.
			if cond.Op == influxql.OR && (lhsTime != nil || rhsTime != nil) {
				return nil, nil, errors.New("cannot use OR with time conditions")
			}
			timeRange := lhsTime.Intersect(rhsTime)

			// Combine the left and right expression.
			if rhsExpr == nil {
				return lhsExpr, timeRange, nil
			} else if lhsExpr == nil {
				return rhsExpr, timeRange, nil
			}
			return &influxql.BinaryExpr{
				Op:  cond.Op,
				LHS: lhsExpr,
				RHS: rhsExpr,
			}, timeRange, nil
		}

		// If either the left or the right side is "time", we are looking at
		// a time range.
		if lhs, ok := cond.LHS.(*influxql.VarRef); ok && lhs.Val == "time" {
			timeRange, err := getTimeRange(cond.Op, cond.RHS, valuer)
			return nil, timeRange, err
		} else if rhs, ok := cond.RHS.(*influxql.VarRef); ok && rhs.Val == "time" {
			// Swap the op for the opposite if it is a comparison.
			op := cond.Op
			switch op {
			case influxql.GT:
				op = influxql.LT
			case influxql.LT:
				op = influxql.GT
			case influxql.GTE:
				op = influxql.LTE
			case influxql.LTE:
				op = influxql.GTE
			}
			timeRange, err := getTimeRange(op, cond.LHS, valuer)
			return nil, timeRange, err
		}
		return cond, nil, nil
	case *influxql.ParenExpr:
		return ParseCondition(cond.Expr, valuer)
	default:
		return nil, nil, fmt.Errorf("invalid condition expression: %s", cond)
	}
}

// getTimeRange returns the time range associated with this comparison.
// op is the operation that is used for comparison and rhs is the right hand side
// of the expression. The left hand side is always assumed to be "time".
func getTimeRange(op influxql.Token, rhs influxql.Expr, valuer influxql.Valuer) (*TimeRange, error) {
	// If literal looks like a date time then parse it as a time literal.
	if strlit, ok := rhs.(*influxql.StringLiteral); ok {
		if strlit.IsTimeLiteral() {
			t, err := strlit.ToTimeLiteral()
			if err != nil {
				return nil, err
			}
			rhs = t
		}
	}

	// Evaluate the RHS to replace "now()" with the current time.
	rhs = influxql.Reduce(rhs, valuer)

	var value time.Time
	switch lit := rhs.(type) {
	case *influxql.TimeLiteral:
		if lit.Val.After(time.Unix(0, influxql.MaxTime)) {
			return nil, fmt.Errorf("time %s overflows time literal", lit.Val.Format(time.RFC3339))
		} else if lit.Val.Before(time.Unix(0, influxql.MinTime+1)) {
			// The minimum allowable time literal is one greater than the minimum time because the minimum time
			// is a sentinel value only used internally.
			return nil, fmt.Errorf("time %s underflows time literal", lit.Val.Format(time.RFC3339))
		}
		value = lit.Val
	case *influxql.DurationLiteral:
		value = time.Unix(0, int64(lit.Val)).UTC()
	case *influxql.NumberLiteral:
		value = time.Unix(0, int64(lit.Val)).UTC()
	case *influxql.IntegerLiteral:
		value = time.Unix(0, lit.Val).UTC()
	default:
		return nil, fmt.Errorf("invalid operation: time and %T are not compatible", lit)
	}

	timeRange := &TimeRange{}
	switch op {
	case influxql.GT:
		timeRange.Min = value.Add(time.Nanosecond)
	case influxql.GTE:
		timeRange.Min = value
	case influxql.LT:
		timeRange.Max = value.Add(-time.Nanosecond)
	case influxql.LTE:
		timeRange.Max = value
	case influxql.EQ:
		timeRange.Min, timeRange.Max = value, value
	default:
		return nil, fmt.Errorf("invalid time comparison operator: %s", op)
	}
	return timeRange, nil
}

func (c *compiledStatement) Select(plan *Plan) ([]*ReadEdge, []string, error) {
	// Map the shards using the time range and sources.
	opt := influxql.SelectOptions{MinTime: c.TimeRange.Min, MaxTime: c.TimeRange.Max}
	mapper, err := plan.ShardMapper.MapShards(c.Sources, &opt)
	if err != nil {
		return nil, nil, err
	}
	defer mapper.Close()

	// Resolve each of the symbols. This resolves wildcards and any types.
	// The number of fields returned may be greater than the currently known
	// number of fields because of wildcards or it could be less.
	fields := make([]*compiledField, 0, len(c.Fields))
	for _, f := range c.Fields {
		outputs, err := c.link(f, mapper)
		if err != nil {
			return nil, nil, err
		}
		fields = append(fields, outputs...)
	}
	c.linkAuxiliaryFields()

	// Determine the names for each field and fill the output slice.
	columns := columnNames(fields)
	outputs := make([]*ReadEdge, len(fields))
	for i, f := range fields {
		outputs[i] = f.Output
	}

	// Add all of the field outputs to the plan as targets.
	for _, out := range outputs {
		plan.AddTarget(out)
	}
	return outputs, columns, nil
}

func (c *compiledStatement) link(f *compiledField, m ShardGroup) ([]*compiledField, error) {
	// Resolve the wildcards for this field if they exist.
	if f.Wildcard != nil {
		fields, err := f.resolveWildcards(m)
		if err != nil {
			return nil, err
		}

		for _, f := range fields {
			f.resolveSymbols(m)
		}
		return fields, nil
	}

	// Resolve all of the symbols for this field.
	f.resolveSymbols(m)
	return []*compiledField{f}, nil
}

func (c *compiledField) resolveWildcards(m ShardGroup) ([]*compiledField, error) {
	// Retrieve the field dimensions from the shard mapper.
	fields, dimensions, err := influxql.FieldDimensions(c.global.Sources, m)
	if err != nil {
		return nil, err
	}

	// Filter out any dimensions that are included in the dimensions.
	for _, d := range c.global.Dimensions {
		delete(dimensions, d)
	}
	// Add the remaining dimensions to the field mapping.
	for d := range dimensions {
		fields[d] = influxql.Tag
	}
	dimensions = nil

	// Filter out any fields that do not pass the filter or are not an allowed type.
	for key, typ := range fields {
		if typ == influxql.Tag {
			typ = influxql.String
		}
		if _, ok := c.Wildcard.TypeFilters[typ]; ok {
			// Filter out this type since it is not compatible with something.
			delete(fields, key)
			continue
		}

		// If a single filter fails, then delete this field.
		for _, filter := range c.Wildcard.NameFilters {
			if !filter.MatchString(key) {
				delete(fields, key)
				break
			}
		}
	}

	// Exit early if there are no matching fields/tags.
	if len(fields) == 0 {
		return nil, nil
	}

	// Sort the field names.
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	// Clone the compiled field once for every wildcard expansion.
	clones := make([]*compiledField, 0, len(names))
	for _, name := range names {
		clone := c.Clone()
		clone.WildcardRef = &influxql.VarRef{
			Val:  name,
			Type: fields[name],
		}
		clones = append(clones, clone)
	}
	return clones, nil
}

// Name returns the name for this field.
func (c *compiledField) Name() string {
	name := c.Field.Name()
	if c.WildcardRef != nil {
		if name != "" {
			name = fmt.Sprintf("%s_%s", name, c.WildcardRef.Val)
		} else {
			name = c.WildcardRef.Val
		}
	}
	return name
}

// requireAuxiliaryFields signals to the global state that we will need
// auxiliary fields to resolve some of the symbols. Instantiating it here lets
// us return an error if auxiliary fields are not compatible with some other
// part of the global state before we start contacting the shards for type
// information.
func (c *compiledStatement) requireAuxiliaryFields() {
	if c.AuxiliaryFields == nil {
		c.AuxiliaryFields = &AuxiliaryFields{}
	}
}

// columnNames processes the column names for the list of compiled fields
// and resolves any conflicts. Aliases are always given highest priority
// regardless of where they are in the statement.
func columnNames(fields []*compiledField) []string {
	columns := make([]string, len(fields))

	// Keep track of the encountered column names.
	names := make(map[string]int)

	// Resolve aliases first.
	for i, col := range fields {
		if col.Field.Alias != "" {
			name := col.Name()
			columns[i] = name
			names[name] = 1
		}
	}

	// Resolve any generated names and resolve conflicts.
	for i, col := range fields {
		if columns[i] != "" {
			continue
		}

		name := col.Name()
		count, conflict := names[name]
		if conflict {
			for {
				resolvedName := fmt.Sprintf("%s_%d", name, count)
				_, conflict = names[resolvedName]
				if !conflict {
					names[name] = count + 1
					name = resolvedName
					break
				}
				count++
			}
		}
		names[name]++
		columns[i] = name
	}
	return columns
}
