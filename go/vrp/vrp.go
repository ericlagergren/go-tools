// Package vrp implements value range analysis on Go programs in SSI form.
//
// We implement the algorithm shown in the paper "Speed And Precision in Range Analysis" by Campos et al. Further resources discussing this algorithm are:
// - Scalable and precise range analysis on the interval lattice by Rodrigues
// - A Fast and Low Overhead Technique to Secure Programs Against Integer Overflows by Rodrigues et al
// - https://github.com/vhscampos/range-analysis
// - https://www.youtube.com/watch?v=Vj-TI4Yjt10
//
// TODO: document use of jump-set widening, possible use of rounds of abstract interpretation, what our lattice looks like, ...
package vrp

// XXX right now our results aren't stable and change depending on the order in which we iterate over maps. why?

// OPT: constants have fixed intervals, they don't need widening or narrowing or fixpoints

// TODO: support more than one interval per value. For example, we should be able to represent the set {0, 10, 100}
// without ending up with [0, 100].

// Our handling of overflow is poor. We basically use saturated integers and when x <op> y overflows, it will be set to
// -∞ or ∞, depending if it's the lower or upper bound of an interval. This means that an interval like [1, ∞] for a
// signed integer really means that the value can be anywhere between its minimum and maximum value. For example, for a
// int8, [1, ∞] really means [-128, 127]. The reason we use [1, ∞] and not [-128, 127] or [-∞, ∞] is that our intervals
// encode growth in the lattice of intervals. In our case, the value only ever overflowed because the upper bound
// overflowed. Note that it is possible for [-∞, -100] - 100 to result in [-∞, ∞], which makes sense in the lattice, but
// doesn't really encode how the overflow happens at runtime.
//
// Nevertheless, if we used more than one interval per variable we could encode tighter bounds. For example, [5, 127] +
// 1 ought to be [6, 127] ∪ [-128, -128].

import (
	"fmt"
	"go/token"
	"go/types"
	"math"
	"sort"

	"honnef.co/go/tools/go/ir"
)

const debug = true

var Inf Numeric = Infinity{}
var NegInf Numeric = Infinity{negative: true}
var Empty = NewInterval(Inf, NegInf)

func Keys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func SortedKeys[K comparable, V any](m map[K]V, less func(a, b K) bool) []K {
	keys := Keys(m)
	sort.Slice(keys, func(i, j int) bool {
		return less(keys[i], keys[j])
	})
	return keys
}

type Numeric interface {
	Cmp(other Numeric) int
	String() string
	Negative() bool
	Add(Numeric) (Numeric, bool)
	Sub(Numeric) (Numeric, bool)
	Inc() (Numeric, bool)
	Dec() (Numeric, bool)
}

type Infinity struct {
	negative bool
}

func (v Infinity) Negative() bool { return v.negative }

func (v Infinity) Cmp(other Numeric) int {
	if other, ok := other.(Infinity); ok {
		if v == other {
			return 0
		} else if v.negative {
			return -1
		} else {
			return 1
		}
	} else {
		if v.negative {
			return -1
		} else {
			return 1
		}
	}
}

func (v Infinity) Add(other Numeric) (Numeric, bool) {
	if v.negative {
		panic("-∞ + y is not defined")
	}
	return v, false
}

func (v Infinity) Sub(other Numeric) (Numeric, bool) {
	if v.negative {
		panic("-∞ - y is not defined")
	}
	return v, false
}

func (v Infinity) Inc() (Numeric, bool) { return v, false }
func (v Infinity) Dec() (Numeric, bool) { return v, false }

func (v Infinity) String() string {
	if v.negative {
		return "-∞"
	} else {
		return "∞"
	}
}

type Interval struct {
	Lower, Upper Numeric
}

func NewInterval(l, u Numeric) Interval {
	if l == nil && u != nil || l != nil && u == nil {
		panic("inconsistent interval")
	}

	return Interval{l, u}
}

func (ival Interval) Empty() bool {
	if ival.Undefined() {
		return false
	}
	if ival.Upper.Cmp(ival.Lower) == -1 {
		return true
	}
	return false
}

// XXX rename this method; it's not a traditional interval union, in which [1, 2] ∪ [4, 5] would be {1, 2, 4, 5}, not [1, 5]
func (ival Interval) Union(oval Interval) Interval {
	if ival.Empty() {
		return oval
	} else if oval.Empty() {
		return ival
	} else if ival.Undefined() {
		return oval
	} else if oval.Undefined() {
		return ival
	} else {
		var l, u Numeric
		if ival.Lower.Cmp(oval.Lower) == -1 {
			l = ival.Lower
		} else {
			l = oval.Lower
		}

		if ival.Upper.Cmp(oval.Upper) == 1 {
			u = ival.Upper
		} else {
			u = oval.Upper
		}

		return NewInterval(l, u)
	}
}

func (ival Interval) Intersect(oval Interval) Interval {
	if ival.Empty() || oval.Empty() {
		return Empty
	}
	if ival.Undefined() {
		return oval
	}
	if oval.Undefined() {
		return ival
	}

	var l, u Numeric
	if ival.Lower.Cmp(oval.Lower) == 1 {
		l = ival.Lower
	} else {
		l = oval.Lower
	}

	if ival.Upper.Cmp(oval.Upper) == -1 {
		u = ival.Upper
	} else {
		u = oval.Upper
	}

	return NewInterval(l, u)
}

func (ival Interval) Equal(oval Interval) bool {
	return (ival.Lower == nil && oval.Lower == nil) || (ival.Lower != nil && oval.Lower != nil) &&
		(ival.Upper == nil && oval.Upper == nil) || (ival.Upper != nil && oval.Upper != nil) &&
		(ival.Lower.Cmp(oval.Lower) == 0) &&
		(ival.Upper.Cmp(oval.Upper) == 0)
}

func (ival Interval) Undefined() bool {
	if ival.Lower == nil && ival.Upper != nil || ival.Lower != nil && ival.Upper == nil {
		panic("inconsistent interval")
	}
	return ival.Lower == nil
}

func (ival Interval) String() string {
	if ival.Undefined() {
		return "[⊥, ⊥]"
	} else {
		l := ival.Lower.String()
		u := ival.Upper.String()
		return fmt.Sprintf("[%s, %s]", l, u)
	}
}

// TODO: we should be able to represent both intersections using a single type
type Intersection interface {
	String() string
	Interval() Interval
}

type BasicIntersection struct {
	interval Interval
}

func (isec BasicIntersection) String() string {
	return isec.interval.String()
}

func (isec BasicIntersection) Interval() Interval {
	return isec.interval
}

// A SymbolicIntersection represents an intersection with an interval bounded by a comparison instruction between two
// variables. For example, for 'if a < b', in the true branch 'a' will be bounded by [min, b - 1], where 'min' is the
// smallest value representable by 'a'.
type SymbolicIntersection struct {
	Op    token.Token
	Value ir.Value
}

func (isec SymbolicIntersection) String() string {
	l := "-∞"
	u := "∞"
	name := isec.Value.Name()
	switch isec.Op {
	case token.LSS:
		u = name + "-1"
	case token.GTR:
		l = name + "+1"
	case token.LEQ:
		u = name
	case token.GEQ:
		l = name
	case token.EQL:
		l = name
		u = name
	default:
		panic(fmt.Sprintf("unhandled token %s", isec.Op))
	}
	return fmt.Sprintf("[%s, %s]", l, u)
}

func (isec SymbolicIntersection) Interval() Interval {
	// We don't have an interval for this intersection yet. If we did, the SymbolicIntersection wouldn't exist any
	// longer and would've been replaced with a basic intersection.
	return NewInterval(nil, nil)
}

func infinity() Interval {
	// XXX should unsigned integers be [-inf, inf] or [0, inf]?
	return NewInterval(NegInf, Inf)
}

// flipToken flips a binary operator. For example, '>' becomes '<'.
func flipToken(tok token.Token) token.Token {
	switch tok {
	case token.LSS:
		return token.GTR
	case token.GTR:
		return token.LSS
	case token.LEQ:
		return token.GEQ
	case token.GEQ:
		return token.LEQ
	case token.EQL:
		return token.EQL
	case token.NEQ:
		return token.NEQ
	default:
		panic(fmt.Sprintf("unhandled token %v", tok))
	}
}

// negateToken negates a binary operator. For example, '>' becomes '<='.
func negateToken(tok token.Token) token.Token {
	switch tok {
	case token.LSS:
		return token.GEQ
	case token.GTR:
		return token.LEQ
	case token.LEQ:
		return token.GTR
	case token.GEQ:
		return token.LSS
	case token.EQL:
		return token.NEQ
	case token.NEQ:
		return token.EQL
	default:
		panic(fmt.Sprintf("unhandled token %s", tok))
	}
}

type valueSet map[ir.Value]struct{}

type constraintGraph struct {
	// OPT: if we wrap ir.Value in a struct with some fields, then we only need one map, which reduces the number of
	// lookups and the memory usage.

	// Map sigma nodes to their intersections. In SSI form, only sigma nodes will have intersections. Only conditionals
	// cause intersections, and conditionals always cause the creation of sigma nodes for all relevant values.
	intersections map[*ir.Sigma]Intersection
	// The subset of fn's instructions that make up our constraint graph.
	nodes valueSet
	// Map instructions to computed intervals
	intervals map[ir.Value]Interval
	// The graph's strongly connected components. The list of SCCs is sorted in topological order.
	sccs []valueSet
}

func min(a, b Numeric) Numeric {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}

	if a.Cmp(b) <= 0 {
		return a
	} else {
		return b
	}
}

func max(a, b Numeric) Numeric {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}

	if a.Cmp(b) >= 0 {
		return a
	} else {
		return b
	}
}

func isInteger(typ types.Type) bool {
	basic, ok := typ.Underlying().(*types.Basic)
	if !ok {
		return false
	}
	return (basic.Info() & types.IsInteger) != 0
}

func minInt(typ types.Type) Numeric {
	// OPT reuse variables for these constants

	basic := typ.Underlying().(*types.Basic)
	switch basic.Kind() {
	case types.Int:
		// XXX don't pretend that everything runs on 64 bit
		return Int[int64]{math.MinInt64}
	case types.Int8:
		return Int[int8]{math.MinInt8}
	case types.Int16:
		return Int[int16]{math.MinInt16}
	case types.Int32:
		return Int[int32]{math.MinInt32}
	case types.Int64:
		return Int[int64]{math.MinInt64}
	case types.Uint:
		// XXX don't pretend that everything runs on 64 bit
		return Uint[uint64]{0}
	case types.Uint8:
		return Uint[uint8]{0}
	case types.Uint16:
		return Uint[uint16]{0}
	case types.Uint32:
		return Uint[uint32]{0}
	case types.Uint64:
		return Uint[uint64]{0}
	case types.Uintptr:
		// XXX don't pretend that everything runs on 64 bit
		return Uint[uint64]{0}
	default:
		panic(fmt.Sprintf("unhandled type %v", basic.Kind()))
	}
}

func maxInt(typ types.Type) Numeric {
	// OPT reuse variables for these constants

	basic := typ.Underlying().(*types.Basic)
	switch basic.Kind() {
	case types.Int:
		// XXX don't pretend that everything runs on 64 bit
		return Int[int64]{math.MaxInt64}
	case types.Int8:
		return Int[int8]{math.MaxInt8}
	case types.Int16:
		return Int[int16]{math.MaxInt16}
	case types.Int32:
		return Int[int32]{math.MaxInt32}
	case types.Int64:
		return Int[int64]{math.MaxInt64}
	case types.Uint:
		// XXX don't pretend that everything runs on 64 bit
		return Uint[uint64]{math.MaxUint64}
	case types.Uint8:
		return Uint[uint8]{math.MaxUint8}
	case types.Uint16:
		return Uint[uint16]{math.MaxUint16}
	case types.Uint32:
		return Uint[uint32]{math.MaxUint32}
	case types.Uint64:
		return Uint[uint64]{math.MaxUint64}
	case types.Uintptr:
		// XXX don't pretend that everything runs on 64 bit
		return Uint[uint64]{math.MaxUint64}
	default:
		panic(fmt.Sprintf("unhandled type %v", basic.Kind()))
	}
}

func buildConstraintGraph(fn *ir.Function) *constraintGraph {
	cg := constraintGraph{
		intersections: map[*ir.Sigma]Intersection{},
		nodes:         valueSet{},
		intervals:     map[ir.Value]Interval{},
	}

	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			v, ok := instr.(ir.Value)
			if !ok {
				continue
			}
			basic, ok := v.Type().Underlying().(*types.Basic)
			if !ok {
				continue
			}
			if (basic.Info() & types.IsInteger) == 0 {
				continue
			}

			cg.nodes[v] = struct{}{}

			if v, ok := v.(*ir.Sigma); ok {
				cg.intersections[v] = BasicIntersection{interval: infinity()}
				// OPT: we repeat many checks for all sigmas in a basic block, even though most information is the same
				// for all sigmas, and the remaining information only matters for at most two sigmas. It might make
				// sense to either cache most of the computation, or to map from control instruction to sigma node, not
				// the other way around.
				switch ctrl := v.From.Control().(type) {
				case *ir.If:
					cond, ok := ctrl.Cond.(*ir.BinOp)
					if ok {
						lc, _ := cond.X.(*ir.Const)
						rc, _ := cond.Y.(*ir.Const)
						if lc != nil && rc != nil {
							// Comparing two constants, which isn't interesting to us
						} else if (lc != nil && rc == nil) || (lc == nil && rc != nil) {
							// Comparing a variable with a constant
							var variable ir.Value
							var k *ir.Const
							var op token.Token
							if lc != nil {
								// constant on the left side
								variable = cond.Y
								k = lc
								op = flipToken(cond.Op)
							} else {
								// constant on the right side
								variable = cond.X
								k = rc
								op = cond.Op
							}
							if variable == v.X {
								if v.From.Succs[1] == b {
									// We're in the else branch
									op = negateToken(op)
								}
								val := ConstToNumeric(k)
								switch op {
								case token.LSS:
									// [-∞, k-1]
									u, of := val.Dec()
									if of {
										u = Inf
									}
									cg.intersections[v] = BasicIntersection{NewInterval(minInt(variable.Type()), u)}
								case token.GTR:
									// [k+1, ∞]
									l, of := val.Inc()
									if of {
										l = NegInf
									}
									cg.intersections[v] = BasicIntersection{NewInterval(l, maxInt(variable.Type()))}
								case token.LEQ:
									// [-∞, k]
									cg.intersections[v] = BasicIntersection{NewInterval(minInt(variable.Type()), val)}
								case token.GEQ:
									// [k, ∞]
									cg.intersections[v] = BasicIntersection{NewInterval(val, maxInt(variable.Type()))}
								case token.NEQ:
									// We cannot represent this constraint
									// [-∞, ∞]
									cg.intersections[v] = BasicIntersection{infinity()}
								case token.EQL:
									// [k, k]
									cg.intersections[v] = BasicIntersection{NewInterval(val, val)}
								default:
									panic(fmt.Sprintf("unhandled token %s", op))
								}
							} else {
								// Conditional isn't about this variable
							}
						} else if lc == nil && rc == nil {
							// Comparing two variables
							if cond.X == cond.Y {
								// Comparing variable with itself, nothing to do"
							} else if cond.X != v.X && cond.Y != v.X {
								// Conditional isn't about this variable
							} else {
								var variable ir.Value
								var op token.Token
								if cond.X == v.X {
									// Our variable on the left side
									variable = cond.Y
									op = cond.Op
								} else {
									// Our variable on the right side
									variable = cond.X
									op = flipToken(cond.Op)
								}

								if v.From.Succs[1] == b {
									// We're in the else branch
									op = negateToken(op)
								}

								switch op {
								case token.LSS, token.GTR, token.LEQ, token.GEQ, token.EQL:
									cg.intersections[v] = SymbolicIntersection{op, variable}
								case token.NEQ:
									// We cannot represent this constraint
									// [-∞, ∞]
									cg.intersections[v] = BasicIntersection{infinity()}
								default:
									panic(fmt.Sprintf("unhandled token %s", op))
								}
							}
						} else {
							panic("unreachable")
						}
					} else {
						// We don't know how to derive new information from the branch condition.
					}
				// case *ir.ConstantSwitch:
				default:
					panic(fmt.Sprintf("unhandled control %T", ctrl))
				}
			}
		}
	}

	cg.sccs = cg.buildSCCs()
	return &cg
}

func (cg *constraintGraph) fixpoint(scc valueSet, color string, fn func(ir.Value)) {
	worklist := Keys(scc)
	for len(worklist) > 0 {
		// XXX is a LIFO okay or do we need FIFO?
		op := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]
		old := cg.intervals[op]

		fn(op)

		res := cg.intervals[op]
		cg.printSCCs(op, color)
		if !old.Equal(res) {
			for _, ref := range *op.Referrers() {
				if ref, ok := ref.(ir.Value); ok && isInteger(ref.Type()) {
					if _, ok := scc[ref]; ok {
						worklist = append(worklist, ref)
					}
				}
			}
		}
	}
}

func (cg *constraintGraph) widen(op ir.Value) {
	old := cg.intervals[op]
	new := cg.eval(op)

	const simple = 0
	const jumpset = 1
	const infinite = 2
	const mode = simple

	switch mode {
	case simple:
		if old.Undefined() {
			cg.intervals[op] = new
		} else if new.Lower.Cmp(old.Lower) == -1 && new.Upper.Cmp(old.Upper) == 1 {
			cg.intervals[op] = infinity()
		} else if new.Lower.Cmp(old.Lower) == -1 {
			cg.intervals[op] = NewInterval(NegInf, old.Upper)
		} else if new.Upper.Cmp(old.Upper) == 1 {
			cg.intervals[op] = NewInterval(old.Lower, Inf)
		}

	case jumpset:
		panic("not implemented")

	case infinite:
		cg.intervals[op] = NewInterval(min(old.Lower, new.Lower), max(old.Upper, new.Upper))
	}
}

func (cg *constraintGraph) narrow(op ir.Value) {
	// This block is the meet narrowing operator. Narrowing is meant to replace infinites with smaller
	// bounds, but leave other bounds alone. That is, [-∞, 10] can become [0, 10], but not [0, 9] or
	// [-∞, 9]. That's why the code below selects the _wider_ bounds for non-infinities. When the
	// widening operator is implemented correctly, then the bounds shouldn't be able to grow.

	old := cg.intervals[op]

	// OPT: if the bounds aren't able to grow, then why are we doing any comparisons/assigning new
	// intervals? Either we went from an infinity to a narrower bound, or nothing should've changed.
	new := cg.eval(op)

	if old.Lower == NegInf && new.Lower != NegInf {
		cg.intervals[op] = NewInterval(new.Lower, old.Upper)
	} else {
		if old.Lower.Cmp(new.Lower) == 1 {
			cg.intervals[op] = NewInterval(new.Lower, old.Upper)
		}
	}

	if old.Upper == Inf && new.Upper != Inf {
		cg.intervals[op] = NewInterval(old.Lower, new.Upper)
	} else {
		if old.Upper.Cmp(new.Upper) == -1 {
			cg.intervals[op] = NewInterval(old.Lower, new.Upper)
		}
	}
}

func XXX(fn *ir.Function) {
	cg := buildConstraintGraph(fn)
	cg.printSCCs(nil, "")

	// XXX the paper's code "propagates" values to dependent SCCs by evaluating their constraints once, so "that the
	// next SCCs after component will have entry points to kick start the range analysis algorithm". intuitively, this
	// sounds unnecessary, but I haven't looked into what "entry points" are or why we need them. "propagating" means
	// evaluating all uses of the values in the finished SCC, and if they're sigma nodes, marking them as unresolved if
	// they're undefined. "entry points" are variables with ranges that aren't unknown. is this just an optimization?

	for _, scc := range cg.sccs {
		if len(scc) == 0 {
			panic("WTF")
		}

		// OPT: select favourable entry points
		cg.fixpoint(scc, "red", cg.widen)

		// Once we've finished processing the SCC we can propagate the ranges of variables to the symbolic
		// intersections that use them.
		cg.fixIntersects(scc)

		for v := range scc {
			if cg.intervals[v].Undefined() {
				cg.intervals[v] = infinity()
			}
		}

		cg.fixpoint(scc, "green", cg.narrow)
	}

	cg.printSCCs(nil, "")
}

func (cg *constraintGraph) fixIntersects(scc valueSet) {
	// OPT cache this compuation
	futuresUsedBy := map[ir.Value][]*ir.Sigma{}
	for sigma, isec := range cg.intersections {
		if isec, ok := isec.(SymbolicIntersection); ok {
			futuresUsedBy[isec.Value] = append(futuresUsedBy[isec.Value], sigma)
		}
	}
	for v := range scc {
		ival := cg.intervals[v]
		for _, sigma := range futuresUsedBy[v] {
			sval := cg.intervals[sigma]
			symb := cg.intersections[sigma].(SymbolicIntersection)
			svall := sval.Lower
			svalu := sval.Upper
			if sval.Undefined() {
				svall = NegInf
				svalu = Inf
			}
			var newval Interval
			switch symb.Op {
			case token.EQL:
				newval = ival
			case token.LEQ:
				newval = NewInterval(svall, ival.Upper)
			case token.LSS:
				// XXX the branch isn't necessary, -∞ + 1 is still -∞
				if ival.Upper != Inf {
					u, of := ival.Upper.Dec()
					if of {
						u = Inf
					}
					newval = NewInterval(svall, u)
				} else {
					newval = NewInterval(svall, ival.Upper)
				}
			case token.GEQ:
				newval = NewInterval(ival.Lower, svalu)
			case token.GTR:
				// XXX the branch isn't necessary, -∞ + 1 is still -∞
				if ival.Lower != NegInf {
					l, of := ival.Lower.Inc()
					if of {
						l = NegInf
					}
					newval = NewInterval(l, svalu)
				} else {
					newval = NewInterval(ival.Lower, svalu)
				}
			default:
				panic(fmt.Sprintf("unhandled token %s", symb.Op))
			}
			cg.intersections[sigma] = BasicIntersection{interval: newval}
		}
	}
}

func (cg *constraintGraph) printSCCs(activeOp ir.Value, color string) {
	if !debug {
		return
	}

	// We first create subgraphs containing the nodes, then create edges between nodes. Graphviz creates a node the
	// first time it sees it, so doing 'a -> b' in a subgraph would create 'b' in that subgraph, even if it belongs in a
	// different one.
	fmt.Println("digraph{")
	n := 0
	for _, scc := range cg.sccs {
		n++
		fmt.Printf("subgraph cluster_%d {\n", n)
		for _, node := range SortedKeys(scc, func(a, b ir.Value) bool { return a.ID() < b.ID() }) {
			extra := ""
			if node == activeOp {
				extra = ", color=" + color
			}
			if sigma, ok := node.(*ir.Sigma); ok {
				fmt.Printf("%s [label=\"%s = %s ∩ %s ∈ %s\"%s];\n", node.Name(), node.Name(), node, cg.intersections[sigma], cg.intervals[node], extra)
			} else {
				fmt.Printf("%s [label=\"%s = %s ∈ %s\"%s];\n", node.Name(), node.Name(), node, cg.intervals[node], extra)
			}
		}
		fmt.Println("}")
	}
	for _, scc := range cg.sccs {
		for _, node := range SortedKeys(scc, func(a, b ir.Value) bool { return a.ID() < b.ID() }) {
			for _, ref_ := range *node.Referrers() {
				ref, ok := ref_.(ir.Value)
				if !ok {
					continue
				}
				if _, ok := cg.nodes[ref]; !ok {
					continue
				}
				fmt.Printf("%s -> %s\n", node.Name(), ref.Name())
			}
			if node, ok := node.(*ir.Sigma); ok {
				if isec, ok := cg.intersections[node].(SymbolicIntersection); ok {
					fmt.Printf("%s -> %s [style=dashed]\n", isec.Value.Name(), node.Name())
				}
			}
		}
	}
	fmt.Println("}")
}

// sccs returns the constraint graph's strongly connected components, in topological order.
func (cg *constraintGraph) buildSCCs() []valueSet {
	futuresUsedBy := map[ir.Value][]*ir.Sigma{}
	for sigma, isec := range cg.intersections {
		if isec, ok := isec.(SymbolicIntersection); ok {
			futuresUsedBy[isec.Value] = append(futuresUsedBy[isec.Value], sigma)
		}
	}
	index := uint64(1)
	S := []ir.Value{}
	data := map[ir.Value]*struct {
		index   uint64
		lowlink uint64
		onstack bool
	}{}
	var sccs []valueSet

	min := func(a, b uint64) uint64 {
		if a < b {
			return a
		}
		return b
	}

	var strongconnect func(v ir.Value)
	strongconnect = func(v ir.Value) {
		vd, ok := data[v]
		if !ok {
			vd = &struct {
				index   uint64
				lowlink uint64
				onstack bool
			}{}
			data[v] = vd
		}
		vd.index = index
		vd.lowlink = index
		index++
		S = append(S, v)
		vd.onstack = true

		// XXX deduplicate code
		for _, w := range futuresUsedBy[v] {
			if _, ok := cg.nodes[w]; !ok {
				continue
			}
			wd, ok := data[w]
			if !ok {
				wd = &struct {
					index   uint64
					lowlink uint64
					onstack bool
				}{}
				data[w] = wd
			}

			if wd.index == 0 {
				strongconnect(w)
				vd.lowlink = min(vd.lowlink, wd.lowlink)
			} else if wd.onstack {
				vd.lowlink = min(vd.lowlink, wd.lowlink)
			}
		}
		for _, w_ := range *v.Referrers() {
			w, ok := w_.(ir.Value)
			if !ok {
				continue
			}
			if _, ok := cg.nodes[w]; !ok {
				continue
			}
			wd, ok := data[w]
			if !ok {
				wd = &struct {
					index   uint64
					lowlink uint64
					onstack bool
				}{}
				data[w] = wd
			}

			if wd.index == 0 {
				strongconnect(w)
				vd.lowlink = min(vd.lowlink, wd.lowlink)
			} else if wd.onstack {
				vd.lowlink = min(vd.lowlink, wd.lowlink)
			}
		}

		if vd.lowlink == vd.index {
			scc := valueSet{}
			for {
				w := S[len(S)-1]
				S = S[:len(S)-1]
				data[w].onstack = false
				scc[w] = struct{}{}
				if w == v {
					break
				}
			}
			if len(scc) > 0 {
				sccs = append(sccs, scc)
			}
		}
	}

	for v := range cg.nodes {
		if data[v] == nil || data[v].index == 0 {
			strongconnect(v)
		}
	}

	// The output of Tarjan is in reverse topological order. Reverse it to bring it into topological order.
	for i := 0; i < len(sccs)/2; i++ {
		sccs[i], sccs[len(sccs)-i-1] = sccs[len(sccs)-i-1], sccs[i]
	}

	return sccs
}

func (cg *constraintGraph) eval(v ir.Value) Interval {
	switch v := v.(type) {
	case *ir.Const:
		n := ConstToNumeric(v)
		return NewInterval(n, n)

	case *ir.BinOp:
		xval := cg.intervals[v.X]
		yval := cg.intervals[v.Y]

		if xval.Undefined() || yval.Undefined() {
			return NewInterval(nil, nil)
		}

		switch v.Op {
		// XXX so much to implement
		case token.ADD:
			xl := xval.Lower
			xu := xval.Upper
			yl := yval.Lower
			yu := yval.Upper

			l := NegInf
			u := Inf
			var of bool
			if xl != NegInf && yl != NegInf {
				l, of = xl.Add(yl)
				if of {
					l = NegInf
				}
			}

			if xu != Inf && yu != Inf {
				u, of = xu.Add(yu)
				if of {
					u = Inf
				}
			}

			return NewInterval(l, u)

		case token.SUB:
			xval := cg.intervals[v.X]
			yval := cg.intervals[v.Y]

			if xval.Undefined() || yval.Undefined() {
				return NewInterval(nil, nil)
			}

			xl := xval.Lower
			xu := xval.Upper
			yl := yval.Lower
			yu := yval.Upper

			var l, u Numeric
			var of bool
			if xl == NegInf || yu == Inf {
				l = NegInf
			} else {
				l, of = xl.Sub(yu)
				if of {
					l = NegInf
				}
			}

			if xu == Inf || yl == NegInf {
				u = Inf
			} else {
				u, of = xu.Sub(yl)
				if of {
					u = Inf
				}
			}

			return NewInterval(l, u)

		default:
			panic(fmt.Sprintf("unhandled token %s", v.Op))
		}

	case *ir.Phi:
		ret := cg.intervals[v.Edges[0]]
		for _, other := range v.Edges[1:] {
			ret = ret.Union(cg.intervals[other])
		}
		return ret

	case *ir.Sigma:
		if cg.intervals[v.X].Undefined() {
			// If sigma gets evaluated before sigma.X we don't want to return the sigma's intersection, which might be
			// [-∞, ∞] and saturate all instructions using the sigma.
			//
			// XXX can we do this without losing precision?
			return NewInterval(nil, nil)
		}

		return cg.intervals[v.X].Intersect(cg.intersections[v].Interval())

	case *ir.Parameter:
		return NewInterval(minInt(v.Type()), maxInt(v.Type()))

	default:
		panic(fmt.Sprintf("unhandled type %T", v))
	}
}