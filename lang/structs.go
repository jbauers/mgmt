// Mgmt
// Copyright (C) 2013-2018+ James Shubin and the project contributors
// Written by James Shubin <james@shubin.ca> and the project contributors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package lang // TODO: move this into a sub package of lang/$name?

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/purpleidea/mgmt/engine"
	engineUtil "github.com/purpleidea/mgmt/engine/util"
	"github.com/purpleidea/mgmt/lang/funcs"
	"github.com/purpleidea/mgmt/lang/funcs/bindata"
	"github.com/purpleidea/mgmt/lang/funcs/structs"
	"github.com/purpleidea/mgmt/lang/interfaces"
	"github.com/purpleidea/mgmt/lang/types"
	"github.com/purpleidea/mgmt/lang/unification"
	"github.com/purpleidea/mgmt/pgraph"
	"github.com/purpleidea/mgmt/util"

	errwrap "github.com/pkg/errors"
	"golang.org/x/time/rate"
)

const (
	// EdgeNotify declares an edge a -> b, such that a notification occurs.
	// This is most similar to "notify" in Puppet.
	EdgeNotify = "notify"

	// EdgeBefore declares an edge a -> b, such that no notification occurs.
	// This is most similar to "before" in Puppet.
	EdgeBefore = "before"

	// EdgeListen declares an edge a <- b, such that a notification occurs.
	// This is most similar to "subscribe" in Puppet.
	EdgeListen = "listen"

	// EdgeDepend declares an edge a <- b, such that no notification occurs.
	// This is most similar to "require" in Puppet.
	EdgeDepend = "depend"

	// MetaField is the prefix used to specify a meta parameter for the res.
	MetaField = "meta"

	// AllowUserDefinedPolyFunc specifies if we allow user-defined
	// polymorphic functions or not. At the moment this is not implemented.
	// XXX: not implemented
	AllowUserDefinedPolyFunc = false

	// RequireStrictModulePath can be set to true if you wish to ignore any
	// of the metadata parent path searching. By default that is allowed,
	// unless it is disabled per module with ParentPathBlock. This option is
	// here in case we decide that the parent module searching is confusing.
	RequireStrictModulePath = false
)

// StmtBind is a representation of an assignment, which binds a variable to an
// expression.
type StmtBind struct {
	Ident string
	Value interfaces.Expr
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtBind) Apply(fn func(interfaces.Node) error) error {
	if err := obj.Value.Apply(fn); err != nil {
		return err
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtBind) Init(data *interfaces.Data) error {
	return obj.Value.Init(data)
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *StmtBind) Interpolate() (interfaces.Stmt, error) {
	interpolated, err := obj.Value.Interpolate()
	if err != nil {
		return nil, err
	}
	return &StmtBind{
		Ident: obj.Ident,
		Value: interpolated,
	}, nil
}

// SetScope sets the scope of the child expression bound to it. It seems this is
// necessary in order to reach this, in particular in situations when a bound
// expression points to a previously bound expression.
func (obj *StmtBind) SetScope(scope *interfaces.Scope) error {
	return obj.Value.SetScope(scope)
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtBind) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	invars, err := obj.Value.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, invars...)

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This particular bind statement adds its linked expression to
// the graph. It is not logically done in the ExprVar since that could exist
// multiple times for the single binding operation done here.
func (obj *StmtBind) Graph() (*pgraph.Graph, error) {
	return obj.Value.Graph()
}

// Output for the bind statement produces no output. Any values of interest come
// from the use of the var which this binds the expression to.
func (obj *StmtBind) Output() (*interfaces.Output, error) {
	return interfaces.EmptyOutput(), nil
}

// StmtRes is a representation of a resource and possibly some edges. The `Name`
// value can be a single string or a list of strings. The former will produce a
// single resource, the latter produces a list of resources. Using this list
// mechanism is a safe alternative to traditional flow control like `for` loops.
// TODO: Consider expanding Name to have this return a list of Res's in the
// Output function if it is a map[name]struct{}, or even a map[[]name]struct{}.
type StmtRes struct {
	data *interfaces.Data

	Kind     string            // kind of resource, eg: pkg, file, svc, etc...
	Name     interfaces.Expr   // unique name for the res of this kind
	Contents []StmtResContents // list of fields/edges in parsed order
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtRes) Apply(fn func(interfaces.Node) error) error {
	if err := obj.Name.Apply(fn); err != nil {
		return err
	}
	for _, x := range obj.Contents {
		if err := x.Apply(fn); err != nil {
			return err
		}
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtRes) Init(data *interfaces.Data) error {
	if strings.Contains(obj.Kind, "_") {
		return fmt.Errorf("kind must not contain underscores")
	}

	obj.data = data
	if err := obj.Name.Init(data); err != nil {
		return err
	}
	for _, x := range obj.Contents {
		if err := x.Init(data); err != nil {
			return err
		}
	}
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *StmtRes) Interpolate() (interfaces.Stmt, error) {
	name, err := obj.Name.Interpolate()
	if err != nil {
		return nil, err
	}

	contents := []StmtResContents{}
	for _, x := range obj.Contents { // make sure we preserve ordering...
		interpolated, err := x.Interpolate()
		if err != nil {
			return nil, err
		}
		contents = append(contents, interpolated)
	}

	return &StmtRes{
		data:     obj.data,
		Kind:     obj.Kind,
		Name:     name,
		Contents: contents,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *StmtRes) SetScope(scope *interfaces.Scope) error {
	if err := obj.Name.SetScope(scope); err != nil {
		return err
	}
	for _, x := range obj.Contents {
		if err := x.SetScope(scope); err != nil {
			return err
		}
	}
	return nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtRes) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	invars, err := obj.Name.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, invars...)

	// name must be a string or a list
	ors := []interfaces.Invariant{}

	invarStr := &unification.EqualsInvariant{
		Expr: obj.Name,
		Type: types.TypeStr,
	}
	ors = append(ors, invarStr)

	invarListStr := &unification.EqualsInvariant{
		Expr: obj.Name,
		Type: types.NewType("[]str"),
	}
	ors = append(ors, invarListStr)

	invar := &unification.ExclusiveInvariant{
		Invariants: ors, // one and only one of these should be true
	}
	invariants = append(invariants, invar)

	// collect all the invariants of each field and edge
	for _, x := range obj.Contents {
		invars, err := x.Unify(obj.Kind) // pass in the resource kind
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invars...)
	}

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. It is interesting to note that nothing directly adds an edge
// to the resources created, but rather, once all the values (expressions) with
// no outgoing edges have produced at least a single value, then the resources
// know they're able to be built.
func (obj *StmtRes) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("res")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}

	g, err := obj.Name.Graph()
	if err != nil {
		return nil, err
	}
	graph.AddGraph(g)

	for _, x := range obj.Contents {
		g, err := x.Graph()
		if err != nil {
			return nil, err
		}
		graph.AddGraph(g)
	}

	return graph, nil
}

// Output returns the output that this "program" produces. This output is what
// is used to build the output graph. This only exists for statements. The
// analogous function for expressions is Value. Those Value functions might get
// called by this Output function if they are needed to produce the output. In
// the case of this resource statement, this is definitely the case.
func (obj *StmtRes) Output() (*interfaces.Output, error) {
	nameValue, err := obj.Name.Value()
	if err != nil {
		return nil, err
	}

	names := []string{} // list of names to build
	switch {
	case types.TypeStr.Cmp(nameValue.Type()) == nil:
		name := nameValue.Str() // must not panic
		names = append(names, name)

	case types.NewType("[]str").Cmp(nameValue.Type()) == nil:
		for _, x := range nameValue.List() { // must not panic
			name := x.Str() // must not panic
			names = append(names, name)
		}

	default:
		// programming error
		return nil, fmt.Errorf("unhandled resource name type: %+v", nameValue.Type())
	}

	resources := []engine.Res{}
	edges := []*interfaces.Edge{}
	for _, name := range names {
		res, err := obj.resource(name)
		if err != nil {
			return nil, errwrap.Wrapf(err, "error building resource")
		}

		edgeList, err := obj.edges(name)
		if err != nil {
			return nil, errwrap.Wrapf(err, "error building edges")
		}
		edges = append(edges, edgeList...)

		if err := obj.metaparams(res); err != nil { // set metaparams
			return nil, errwrap.Wrapf(err, "error building meta params")
		}
		resources = append(resources, res)
	}

	return &interfaces.Output{
		Resources: resources,
		Edges:     edges,
	}, nil
}

// resource is a helper function to generate the res that comes from this.
// TODO: it could memoize some of the work to avoid re-computation when looped
func (obj *StmtRes) resource(resName string) (engine.Res, error) {
	res, err := engine.NewNamedResource(obj.Kind, resName)
	if err != nil {
		return nil, errwrap.Wrapf(err, "cannot create resource kind `%s` with named `%s`", obj.Kind, resName)
	}

	s := reflect.ValueOf(res).Elem() // pointer to struct, then struct
	if k := s.Kind(); k != reflect.Struct {
		panic(fmt.Sprintf("expected struct, got: %s", k))
	}

	mapping, err := engineUtil.LangFieldNameToStructFieldName(obj.Kind)
	if err != nil {
		return nil, err
	}
	ts := reflect.TypeOf(res).Elem() // pointer to struct, then struct

	// FIXME: we could probably simplify this code...
	for _, line := range obj.Contents {
		x, ok := line.(*StmtResField)
		if !ok {
			continue
		}

		if x.Condition != nil {
			b, err := x.Condition.Value()
			if err != nil {
				return nil, err
			}

			if !b.Bool() { // if value exists, and is false, skip it
				continue
			}
		}

		typ, err := x.Value.Type()
		if err != nil {
			return nil, errwrap.Wrapf(err, "resource field `%s` did not return a type", x.Field)
		}

		fieldValue, err := x.Value.Value() // Value method on Expr
		if err != nil {
			return nil, err
		}
		val := fieldValue.Value() // get interface value

		name, exists := mapping[x.Field] // lookup recommended field name
		if !exists {
			return nil, fmt.Errorf("field `%s` does not exist", x.Field) // user made a typo?
		}

		f := s.FieldByName(name) // exported field
		if !f.IsValid() || !f.CanSet() {
			return nil, fmt.Errorf("field `%s` cannot be set", name) // field is broken?
		}

		tf, exists := ts.FieldByName(name) // exported field type
		if !exists {                       // illogical because of above check?
			return nil, fmt.Errorf("field `%s` type does not exist", x.Field)
		}

		// is expr type compatible with expected field type?
		t, err := types.TypeOf(tf.Type)
		if err != nil {
			return nil, errwrap.Wrapf(err, "resource field `%s` has no compatible type", x.Field)
		}
		if err := t.Cmp(typ); err != nil {
			return nil, errwrap.Wrapf(err, "resource field `%s` of type `%+v`, cannot take type `%+v", x.Field, t, typ)
		}

		// user `pestle` on #go-nuts irc wrongly insisted that it wasn't
		// right to use reflect to do all of this. what is a better way?

		// first iterate through the raw pointers to the underlying type
		ttt := tf.Type // ttt is field expected type
		tkk := ttt.Kind()
		for tkk == reflect.Ptr {
			ttt = ttt.Elem() // un-nest one pointer
			tkk = ttt.Kind()
		}

		// all our int's are src kind == reflect.Int64 in our language!
		if obj.data.Debug {
			obj.data.Logf("field `%s`: type(%+v), expected(%+v)", x.Field, typ, tkk)
		}

		// overflow check
		switch tkk { // match on destination field kind
		case reflect.Int, reflect.Int64, reflect.Int32, reflect.Int16, reflect.Int8:
			ff := reflect.Zero(ttt)          // test on a non-ptr equivalent
			if ff.OverflowInt(val.(int64)) { // this is valid!
				return nil, fmt.Errorf("field `%s` is an `%s`, and value `%d` will overflow it", x.Field, f.Kind(), val)
			}

		case reflect.Uint, reflect.Uint64, reflect.Uint32, reflect.Uint16, reflect.Uint8:
			ff := reflect.Zero(ttt)
			if ff.OverflowUint(uint64(val.(int64))) { // TODO: is this correct?
				return nil, fmt.Errorf("field `%s` is an `%s`, and value `%d` will overflow it", x.Field, f.Kind(), val)
			}

		case reflect.Float64, reflect.Float32:
			ff := reflect.Zero(ttt)
			if ff.OverflowFloat(val.(float64)) {
				return nil, fmt.Errorf("field `%s` is an `%s`, and value `%d` will overflow it", x.Field, f.Kind(), val)
			}
		}

		value := reflect.ValueOf(val) // raw value
		value = value.Convert(ttt)    // now convert our raw value properly

		// finally build a new value to set
		tt := tf.Type
		kk := tt.Kind()
		if obj.data.Debug {
			obj.data.Logf("field `%s`: start(%v)->kind(%v)", x.Field, tt, kk)
		}
		//fmt.Printf("start: %v || %+v\n", tt, kk)
		for kk == reflect.Ptr {
			tt = tt.Elem() // un-nest one pointer
			kk = tt.Kind()
			if obj.data.Debug {
				obj.data.Logf("field `%s`:\tloop(%v)->kind(%v)", x.Field, tt, kk)
			}
			// wrap in ptr by one level
			valof := reflect.ValueOf(value.Interface())
			value = reflect.New(valof.Type())
			value.Elem().Set(valof)
		}
		f.Set(value) // set it !
	}

	return res, nil
}

// edges is a helper function to generate the edges that come from the resource.
func (obj *StmtRes) edges(resName string) ([]*interfaces.Edge, error) {
	edges := []*interfaces.Edge{}

	// to and from self, map of kind, name, notify
	var to = make(map[string]map[string]bool)   // to this from self
	var from = make(map[string]map[string]bool) // from this to self

	for _, line := range obj.Contents {
		x, ok := line.(*StmtResEdge)
		if !ok {
			continue
		}

		if x.Condition != nil {
			b, err := x.Condition.Value()
			if err != nil {
				return nil, err
			}

			if !b.Bool() { // if value exists, and is false, skip it
				continue
			}
		}

		nameValue, err := x.EdgeHalf.Name.Value()
		if err != nil {
			return nil, err
		}

		// the edge name can be a single string or a list of strings...

		names := []string{} // list of names to build
		switch {
		case types.TypeStr.Cmp(nameValue.Type()) == nil:
			name := nameValue.Str() // must not panic
			names = append(names, name)

		case types.NewType("[]str").Cmp(nameValue.Type()) == nil:
			for _, x := range nameValue.List() { // must not panic
				name := x.Str() // must not panic
				names = append(names, name)
			}

		default:
			// programming error
			return nil, fmt.Errorf("unhandled resource name type: %+v", nameValue.Type())
		}

		kind := x.EdgeHalf.Kind
		for _, name := range names {
			var notify bool

			switch p := x.Property; p {
			// a -> b
			// a notify b
			// a before b
			case EdgeNotify:
				notify = true
				fallthrough
			case EdgeBefore:
				if m, exists := to[kind]; !exists {
					to[kind] = make(map[string]bool)
				} else if n, exists := m[name]; exists {
					notify = notify || n // collate
				}
				to[kind][name] = notify // to this from self

			// b -> a
			// b listen a
			// b depend a
			case EdgeListen:
				notify = true
				fallthrough
			case EdgeDepend:
				if m, exists := from[kind]; !exists {
					from[kind] = make(map[string]bool)
				} else if n, exists := m[name]; exists {
					notify = notify || n // collate
				}
				from[kind][name] = notify // from this to self

			default:
				return nil, fmt.Errorf("unknown property: %s", p)
			}
		}
	}

	// TODO: we could detect simple loops here (if `from` and `to` have the
	// same entry) but we can leave this to the proper dag checker later on

	for kind, x := range to { // to this from self
		for name, notify := range x {
			edge := &interfaces.Edge{
				Kind1: obj.Kind,
				Name1: resName, // self
				//Send: "",

				Kind2: kind,
				Name2: name,
				//Recv: "",

				Notify: notify,
			}
			edges = append(edges, edge)
		}
	}
	for kind, x := range from { // from this to self
		for name, notify := range x {
			edge := &interfaces.Edge{
				Kind1: kind,
				Name1: name,
				//Send: "",

				Kind2: obj.Kind,
				Name2: resName, // self
				//Recv: "",

				Notify: notify,
			}
			edges = append(edges, edge)
		}
	}

	return edges, nil
}

// metaparams is a helper function to set the metaparams that come from the
// resource on to the individual resource we're working on.
func (obj *StmtRes) metaparams(res engine.Res) error {
	meta := engine.DefaultMetaParams.Copy() // defaults

	var aem *engine.AutoEdgeMeta
	if r, ok := res.(engine.EdgeableRes); ok {
		aem = r.AutoEdgeMeta() // get a struct with the defaults
	}
	var agm *engine.AutoGroupMeta
	if r, ok := res.(engine.GroupableRes); ok {
		agm = r.AutoGroupMeta() // get a struct with the defaults
	}

	for _, line := range obj.Contents {
		x, ok := line.(*StmtResMeta)
		if !ok {
			continue
		}

		if x.Condition != nil {
			b, err := x.Condition.Value()
			if err != nil {
				return err
			}

			if !b.Bool() { // if value exists, and is false, skip it
				continue
			}
		}

		v, err := x.MetaExpr.Value()
		if err != nil {
			return err
		}

		switch p := strings.ToLower(x.Property); p {
		// TODO: we could add these fields dynamically if we were fancy!
		case "noop":
			meta.Noop = v.Bool() // must not panic

		case "retry":
			x := v.Int() // must not panic
			// TODO: check that it doesn't overflow
			meta.Retry = int16(x)

		case "delay":
			x := v.Int() // must not panic
			// TODO: check that it isn't signed
			meta.Delay = uint64(x)

		case "poll":
			x := v.Int() // must not panic
			// TODO: check that it doesn't overflow and isn't signed
			meta.Poll = uint32(x)

		case "limit": // rate.Limit
			x := v.Float() // must not panic
			meta.Limit = rate.Limit(x)

		case "burst":
			x := v.Int() // must not panic
			// TODO: check that it doesn't overflow
			meta.Burst = int(x)

		case "sema": // []string
			values := []string{}
			for _, x := range v.List() { // must not panic
				s := x.Str() // must not panic
				values = append(values, s)
			}
			meta.Sema = values

		case "autoedge":
			if aem != nil {
				aem.Disabled = !v.Bool() // must not panic
			}

		case "autogroup":
			if agm != nil {
				agm.Disabled = !v.Bool() // must not panic
			}

		case MetaField:
			if val, exists := v.Struct()["noop"]; exists {
				meta.Noop = val.Bool() // must not panic
			}
			if val, exists := v.Struct()["retry"]; exists {
				x := val.Int() // must not panic
				// TODO: check that it doesn't overflow
				meta.Retry = int16(x)
			}
			if val, exists := v.Struct()["delay"]; exists {
				x := val.Int() // must not panic
				// TODO: check that it isn't signed
				meta.Delay = uint64(x)
			}
			if val, exists := v.Struct()["poll"]; exists {
				x := val.Int() // must not panic
				// TODO: check that it doesn't overflow and isn't signed
				meta.Poll = uint32(x)
			}
			if val, exists := v.Struct()["limit"]; exists {
				x := val.Float() // must not panic
				meta.Limit = rate.Limit(x)
			}
			if val, exists := v.Struct()["burst"]; exists {
				x := val.Int() // must not panic
				// TODO: check that it doesn't overflow
				meta.Burst = int(x)
			}
			if val, exists := v.Struct()["sema"]; exists {
				values := []string{}
				for _, x := range val.List() { // must not panic
					s := x.Str() // must not panic
					values = append(values, s)
				}
				meta.Sema = values
			}
			if val, exists := v.Struct()["autoedge"]; exists && aem != nil {
				aem.Disabled = !val.Bool() // must not panic
			}
			if val, exists := v.Struct()["autogroup"]; exists && agm != nil {
				agm.Disabled = !val.Bool() // must not panic
			}

		default:
			return fmt.Errorf("unknown property: %s", p)
		}
	}

	res.SetMetaParams(meta) // set it!
	if r, ok := res.(engine.EdgeableRes); ok {
		r.SetAutoEdgeMeta(aem) // set
	}
	if r, ok := res.(engine.GroupableRes); ok {
		r.SetAutoGroupMeta(agm) // set
	}

	return nil
}

// StmtResContents is the interface that is met by the resource contents. Look
// closely for while it is similar to the Stmt interface, it is quite different.
type StmtResContents interface {
	interfaces.Node
	Init(*interfaces.Data) error
	Interpolate() (StmtResContents, error) // different!
	SetScope(*interfaces.Scope) error
	Unify(kind string) ([]interfaces.Invariant, error) // different!
	Graph() (*pgraph.Graph, error)
}

// StmtResField represents a single field in the parsed resource representation.
// This does not satisfy the Stmt interface.
type StmtResField struct {
	Field     string
	Value     interfaces.Expr
	Condition interfaces.Expr // the value will be used if nil or true
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtResField) Apply(fn func(interfaces.Node) error) error {
	if obj.Condition != nil {
		if err := obj.Condition.Apply(fn); err != nil {
			return err
		}
	}
	if err := obj.Value.Apply(fn); err != nil {
		return err
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtResField) Init(data *interfaces.Data) error {
	if obj.Condition != nil {
		if err := obj.Condition.Init(data); err != nil {
			return err
		}
	}
	return obj.Value.Init(data)
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// This interpolate is different It is different from the interpolate found in
// the Expr and Stmt interfaces because it returns a different type as output.
func (obj *StmtResField) Interpolate() (StmtResContents, error) {
	interpolated, err := obj.Value.Interpolate()
	if err != nil {
		return nil, err
	}
	var condition interfaces.Expr
	if obj.Condition != nil {
		condition, err = obj.Condition.Interpolate()
		if err != nil {
			return nil, err
		}
	}
	return &StmtResField{
		Field:     obj.Field,
		Value:     interpolated,
		Condition: condition,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *StmtResField) SetScope(scope *interfaces.Scope) error {
	if err := obj.Value.SetScope(scope); err != nil {
		return err
	}
	if obj.Condition != nil {
		if err := obj.Condition.SetScope(scope); err != nil {
			return err
		}
	}
	return nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller. It is different from the Unify found in the Expr
// and Stmt interfaces because it adds an input parameter.
func (obj *StmtResField) Unify(kind string) ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	invars, err := obj.Value.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, invars...)

	// conditional expression might have some children invariants to share
	if obj.Condition != nil {
		condition, err := obj.Condition.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, condition...)

		// the condition must ultimately be a boolean
		conditionInvar := &unification.EqualsInvariant{
			Expr: obj.Condition,
			Type: types.TypeBool,
		}
		invariants = append(invariants, conditionInvar)
	}

	// TODO: unfortunately this gets called separately for each field... if
	// we could cache this, it might be worth looking into for performance!
	typMap, err := engineUtil.LangFieldNameToStructType(kind)
	if err != nil {
		return nil, err
	}

	field := strings.TrimSpace(obj.Field)
	if len(field) != len(obj.Field) {
		return nil, fmt.Errorf("field was wrapped in whitespace")
	}
	if len(strings.Fields(field)) != 1 {
		return nil, fmt.Errorf("field was empty or contained spaces")
	}

	typ, exists := typMap[obj.Field]
	if !exists {
		return nil, fmt.Errorf("could not determine type for `%s` field of `%s`", obj.Field, kind)
	}
	invar := &unification.EqualsInvariant{
		Expr: obj.Value,
		Type: typ,
	}
	invariants = append(invariants, invar)

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. It is interesting to note that nothing directly adds an edge
// to the resources created, but rather, once all the values (expressions) with
// no outgoing edges have produced at least a single value, then the resources
// know they're able to be built.
func (obj *StmtResField) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("resfield")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}

	g, err := obj.Value.Graph()
	if err != nil {
		return nil, err
	}
	graph.AddGraph(g)

	if obj.Condition != nil {
		g, err := obj.Condition.Graph()
		if err != nil {
			return nil, err
		}
		graph.AddGraph(g)
	}

	return graph, nil
}

// StmtResEdge represents a single edge property in the parsed resource
// representation. This does not satisfy the Stmt interface.
type StmtResEdge struct {
	Property  string // TODO: iota constant instead?
	EdgeHalf  *StmtEdgeHalf
	Condition interfaces.Expr // the value will be used if nil or true
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtResEdge) Apply(fn func(interfaces.Node) error) error {
	if obj.Condition != nil {
		if err := obj.Condition.Apply(fn); err != nil {
			return err
		}
	}
	if err := obj.EdgeHalf.Apply(fn); err != nil {
		return err
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtResEdge) Init(data *interfaces.Data) error {
	if obj.Property == "" {
		return fmt.Errorf("empty property")
	}
	if obj.Property != EdgeNotify && obj.Property != EdgeBefore && obj.Property != EdgeListen && obj.Property != EdgeDepend {
		return fmt.Errorf("invalid property: `%s`", obj.Property)
	}

	if obj.Condition != nil {
		if err := obj.Condition.Init(data); err != nil {
			return err
		}
	}
	return obj.EdgeHalf.Init(data)
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// This interpolate is different It is different from the interpolate found in
// the Expr and Stmt interfaces because it returns a different type as output.
func (obj *StmtResEdge) Interpolate() (StmtResContents, error) {
	interpolated, err := obj.EdgeHalf.Interpolate()
	if err != nil {
		return nil, err
	}
	var condition interfaces.Expr
	if obj.Condition != nil {
		condition, err = obj.Condition.Interpolate()
		if err != nil {
			return nil, err
		}
	}
	return &StmtResEdge{
		Property:  obj.Property,
		EdgeHalf:  interpolated,
		Condition: condition,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *StmtResEdge) SetScope(scope *interfaces.Scope) error {
	if err := obj.EdgeHalf.SetScope(scope); err != nil {
		return err
	}
	if obj.Condition != nil {
		if err := obj.Condition.SetScope(scope); err != nil {
			return err
		}
	}
	return nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller. It is different from the Unify found in the Expr
// and Stmt interfaces because it adds an input parameter.
func (obj *StmtResEdge) Unify(kind string) ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	invars, err := obj.EdgeHalf.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, invars...)

	// conditional expression might have some children invariants to share
	if obj.Condition != nil {
		condition, err := obj.Condition.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, condition...)

		// the condition must ultimately be a boolean
		conditionInvar := &unification.EqualsInvariant{
			Expr: obj.Condition,
			Type: types.TypeBool,
		}
		invariants = append(invariants, conditionInvar)
	}

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. It is interesting to note that nothing directly adds an edge
// to the resources created, but rather, once all the values (expressions) with
// no outgoing edges have produced at least a single value, then the resources
// know they're able to be built.
func (obj *StmtResEdge) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("resedge")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}

	g, err := obj.EdgeHalf.Graph()
	if err != nil {
		return nil, err
	}
	graph.AddGraph(g)

	if obj.Condition != nil {
		g, err := obj.Condition.Graph()
		if err != nil {
			return nil, err
		}
		graph.AddGraph(g)
	}

	return graph, nil
}

// StmtResMeta represents a single meta value in the parsed resource
// representation. It can also contain a struct that contains one or more meta
// parameters. If it contains such a struct, then the `Property` field contains
// the string found in the MetaField constant, otherwise this field will
// correspond to the particular meta parameter specified. This does not satisfy
// the Stmt interface.
type StmtResMeta struct {
	Property  string // TODO: iota constant instead?
	MetaExpr  interfaces.Expr
	Condition interfaces.Expr // the value will be used if nil or true
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtResMeta) Apply(fn func(interfaces.Node) error) error {
	if obj.Condition != nil {
		if err := obj.Condition.Apply(fn); err != nil {
			return err
		}
	}
	if err := obj.MetaExpr.Apply(fn); err != nil {
		return err
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtResMeta) Init(data *interfaces.Data) error {
	if obj.Property == "" {
		return fmt.Errorf("empty property")
	}

	switch p := strings.ToLower(obj.Property); p {
	// TODO: we could add these fields dynamically if we were fancy!
	case "noop":
	case "retry":
	case "delay":
	case "poll":
	case "limit":
	case "burst":
	case "sema":
	case "autoedge":
	case "autogroup":
	case MetaField:

	default:
		return fmt.Errorf("invalid property: `%s`", obj.Property)
	}

	if obj.Condition != nil {
		if err := obj.Condition.Init(data); err != nil {
			return err
		}
	}
	return obj.MetaExpr.Init(data)
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// This interpolate is different It is different from the interpolate found in
// the Expr and Stmt interfaces because it returns a different type as output.
func (obj *StmtResMeta) Interpolate() (StmtResContents, error) {
	interpolated, err := obj.MetaExpr.Interpolate()
	if err != nil {
		return nil, err
	}
	var condition interfaces.Expr
	if obj.Condition != nil {
		condition, err = obj.Condition.Interpolate()
		if err != nil {
			return nil, err
		}
	}
	return &StmtResMeta{
		Property:  obj.Property,
		MetaExpr:  interpolated,
		Condition: condition,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *StmtResMeta) SetScope(scope *interfaces.Scope) error {
	if err := obj.MetaExpr.SetScope(scope); err != nil {
		return err
	}
	if obj.Condition != nil {
		if err := obj.Condition.SetScope(scope); err != nil {
			return err
		}
	}
	return nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller. It is different from the Unify found in the Expr
// and Stmt interfaces because it adds an input parameter.
// XXX: Allow specifying partial meta param structs and unify the subset type.
// XXX: The resource fields have the same limitation with field structs.
func (obj *StmtResMeta) Unify(kind string) ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	invars, err := obj.MetaExpr.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, invars...)

	// conditional expression might have some children invariants to share
	if obj.Condition != nil {
		condition, err := obj.Condition.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, condition...)

		// the condition must ultimately be a boolean
		conditionInvar := &unification.EqualsInvariant{
			Expr: obj.Condition,
			Type: types.TypeBool,
		}
		invariants = append(invariants, conditionInvar)
	}

	// add additional invariants based on what's in obj.Property !!!
	var typ *types.Type
	switch p := strings.ToLower(obj.Property); p {
	// TODO: we could add these fields dynamically if we were fancy!
	case "noop":
		typ = types.TypeBool

	case "retry":
		typ = types.TypeInt

	case "delay":
		typ = types.TypeInt

	case "poll":
		typ = types.TypeInt

	case "limit": // rate.Limit
		typ = types.TypeFloat

	case "burst":
		typ = types.TypeInt

	case "sema":
		typ = types.NewType("[]str")

	case "autoedge":
		typ = types.TypeBool

	case "autogroup":
		typ = types.TypeBool

	// autoedge and autogroup aren't part of the `MetaRes` interface, but we
	// can merge them in here for simplicity in the public user interface...
	case MetaField:
		// FIXME: allow partial subsets of this struct, and in any order
		// FIXME: we might need an updated unification engine to do this
		typ = types.NewType("struct{noop bool; retry int; delay int; poll int; limit float; burst int; sema []str; autoedge bool; autogroup bool}")

	default:
		return nil, fmt.Errorf("unknown property: %s", p)
	}
	invar := &unification.EqualsInvariant{
		Expr: obj.MetaExpr,
		Type: typ,
	}
	invariants = append(invariants, invar)

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. It is interesting to note that nothing directly adds an edge
// to the resources created, but rather, once all the values (expressions) with
// no outgoing edges have produced at least a single value, then the resources
// know they're able to be built.
func (obj *StmtResMeta) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("resmeta")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}

	g, err := obj.MetaExpr.Graph()
	if err != nil {
		return nil, err
	}
	graph.AddGraph(g)

	if obj.Condition != nil {
		g, err := obj.Condition.Graph()
		if err != nil {
			return nil, err
		}
		graph.AddGraph(g)
	}

	return graph, nil
}

// StmtEdge is a representation of a dependency. It also supports send/recv.
// Edges represents that the first resource (Kind/Name) listed in the
// EdgeHalfList should happen in the resource graph *before* the next resource
// in the list. If there are multiple StmtEdgeHalf structs listed, then they
// should represent a chain, eg: a->b->c, should compile into a->b & b->c. If
// specified, values are sent and received along these edges if the Send/Recv
// names are compatible and listed. In this case of Send/Recv, only lists of
// length two are legal.
type StmtEdge struct {
	EdgeHalfList []*StmtEdgeHalf // represents a chain of edges

	// TODO: should notify be an Expr?
	Notify bool // specifies that this edge sends a notification as well
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtEdge) Apply(fn func(interfaces.Node) error) error {
	for _, x := range obj.EdgeHalfList {
		if err := x.Apply(fn); err != nil {
			return err
		}
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtEdge) Init(data *interfaces.Data) error {
	for _, x := range obj.EdgeHalfList {
		if err := x.Init(data); err != nil {
			return err
		}
	}
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// TODO: could we expand the Name's from the EdgeHalf (if they're lists) to have
// them return a list of Edges's ?
// XXX: type check the kind1:send -> kind2:recv fields are compatible!
// XXX: we won't know the names yet, but it's okay.
func (obj *StmtEdge) Interpolate() (interfaces.Stmt, error) {
	edgeHalfList := []*StmtEdgeHalf{}
	for _, x := range obj.EdgeHalfList {
		edgeHalf, err := x.Interpolate()
		if err != nil {
			return nil, err
		}
		edgeHalfList = append(edgeHalfList, edgeHalf)
	}

	return &StmtEdge{
		EdgeHalfList: edgeHalfList,
		Notify:       obj.Notify,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *StmtEdge) SetScope(scope *interfaces.Scope) error {
	for _, x := range obj.EdgeHalfList {
		if err := x.SetScope(scope); err != nil {
			return err
		}
	}
	return nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtEdge) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	// TODO: this sort of sideloaded validation could happen in a dedicated
	// Validate() function, but for now is here for lack of a better place!
	if len(obj.EdgeHalfList) == 1 {
		return nil, fmt.Errorf("can't create an edge with only one half")
	}
	if len(obj.EdgeHalfList) == 2 {
		if (obj.EdgeHalfList[0].SendRecv == "") != (obj.EdgeHalfList[1].SendRecv == "") { // xor
			return nil, fmt.Errorf("you must specify both send/recv fields or neither")
		}

		// XXX: check that the kind1:send -> kind2:recv fields are type
		// compatible! XXX: we won't know the names yet, but it's okay.
	}

	for _, x := range obj.EdgeHalfList {
		if x.SendRecv != "" && len(obj.EdgeHalfList) != 2 {
			return nil, fmt.Errorf("send/recv edges must come in pairs")
		}

		invars, err := x.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invars...)
	}

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. It is interesting to note that nothing directly adds an edge
// to the edges created, but rather, once all the values (expressions) with no
// outgoing function graph edges have produced at least a single value, then the
// edges know they're able to be built.
func (obj *StmtEdge) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("edge")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}

	for _, x := range obj.EdgeHalfList {
		g, err := x.Graph()
		if err != nil {
			return nil, err
		}
		graph.AddGraph(g)
	}

	return graph, nil
}

// Output returns the output that this "program" produces. This output is what
// is used to build the output graph. This only exists for statements. The
// analogous function for expressions is Value. Those Value functions might get
// called by this Output function if they are needed to produce the output. In
// the case of this edge statement, this is definitely the case. This edge stmt
// returns output consisting of edges.
func (obj *StmtEdge) Output() (*interfaces.Output, error) {
	edges := []*interfaces.Edge{}

	for i := 0; i < len(obj.EdgeHalfList)-1; i++ {
		nameValue1, err := obj.EdgeHalfList[i].Name.Value()
		if err != nil {
			return nil, err
		}

		// the edge name can be a single string or a list of strings...

		names1 := []string{} // list of names to build
		switch {
		case types.TypeStr.Cmp(nameValue1.Type()) == nil:
			name := nameValue1.Str() // must not panic
			names1 = append(names1, name)

		case types.NewType("[]str").Cmp(nameValue1.Type()) == nil:
			for _, x := range nameValue1.List() { // must not panic
				name := x.Str() // must not panic
				names1 = append(names1, name)
			}

		default:
			// programming error
			return nil, fmt.Errorf("unhandled resource name type: %+v", nameValue1.Type())
		}

		nameValue2, err := obj.EdgeHalfList[i+1].Name.Value()
		if err != nil {
			return nil, err
		}

		names2 := []string{} // list of names to build
		switch {
		case types.TypeStr.Cmp(nameValue2.Type()) == nil:
			name := nameValue2.Str() // must not panic
			names2 = append(names2, name)

		case types.NewType("[]str").Cmp(nameValue2.Type()) == nil:
			for _, x := range nameValue2.List() { // must not panic
				name := x.Str() // must not panic
				names2 = append(names2, name)
			}

		default:
			// programming error
			return nil, fmt.Errorf("unhandled resource name type: %+v", nameValue2.Type())
		}

		for _, name1 := range names1 {
			for _, name2 := range names2 {
				edge := &interfaces.Edge{
					Kind1: obj.EdgeHalfList[i].Kind,
					Name1: name1,
					Send:  obj.EdgeHalfList[i].SendRecv,

					Kind2: obj.EdgeHalfList[i+1].Kind,
					Name2: name2,
					Recv:  obj.EdgeHalfList[i+1].SendRecv,

					Notify: obj.Notify,
				}
				edges = append(edges, edge)
			}
		}
	}

	return &interfaces.Output{
		Edges: edges,
	}, nil
}

// StmtEdgeHalf represents half of an edge in the parsed edge representation.
// This does not satisfy the Stmt interface.
type StmtEdgeHalf struct {
	Kind     string          // kind of resource, eg: pkg, file, svc, etc...
	Name     interfaces.Expr // unique name for the res of this kind
	SendRecv string          // name of field to send/recv from, empty to ignore
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtEdgeHalf) Apply(fn func(interfaces.Node) error) error {
	if err := obj.Name.Apply(fn); err != nil {
		return err
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtEdgeHalf) Init(data *interfaces.Data) error {
	if strings.Contains(obj.Kind, "_") {
		return fmt.Errorf("kind must not contain underscores")
	}

	return obj.Name.Init(data)
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// This interpolate is different It is different from the interpolate found in
// the Expr and Stmt interfaces because it returns a different type as output.
func (obj *StmtEdgeHalf) Interpolate() (*StmtEdgeHalf, error) {
	name, err := obj.Name.Interpolate()
	if err != nil {
		return nil, err
	}

	return &StmtEdgeHalf{
		Kind:     obj.Kind,
		Name:     name,
		SendRecv: obj.SendRecv,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *StmtEdgeHalf) SetScope(scope *interfaces.Scope) error {
	return obj.Name.SetScope(scope)
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtEdgeHalf) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	if obj.Kind == "" {
		return nil, fmt.Errorf("missing resource kind in edge")
	}

	invars, err := obj.Name.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, invars...)

	// name must be a string or a list
	ors := []interfaces.Invariant{}

	invarStr := &unification.EqualsInvariant{
		Expr: obj.Name,
		Type: types.TypeStr,
	}
	ors = append(ors, invarStr)

	invarListStr := &unification.EqualsInvariant{
		Expr: obj.Name,
		Type: types.NewType("[]str"),
	}
	ors = append(ors, invarListStr)

	invar := &unification.ExclusiveInvariant{
		Invariants: ors, // one and only one of these should be true
	}
	invariants = append(invariants, invar)

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. It is interesting to note that nothing directly adds an edge
// to the resources created, but rather, once all the values (expressions) with
// no outgoing edges have produced at least a single value, then the resources
// know they're able to be built.
func (obj *StmtEdgeHalf) Graph() (*pgraph.Graph, error) {
	return obj.Name.Graph()
}

// StmtIf represents an if condition that contains between one and two branches
// of statements to be executed based on the evaluation of the boolean condition
// over time. In particular, this is different from an ExprIf which returns a
// value, where as this produces some Output. Normally if one of the branches is
// optional, it is the else branch, although this struct allows either to be
// optional, even if it is not commonly used.
type StmtIf struct {
	Condition  interfaces.Expr
	ThenBranch interfaces.Stmt // optional, but usually present
	ElseBranch interfaces.Stmt // optional
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtIf) Apply(fn func(interfaces.Node) error) error {
	if err := obj.Condition.Apply(fn); err != nil {
		return err
	}
	if obj.ThenBranch != nil {
		if err := obj.ThenBranch.Apply(fn); err != nil {
			return err
		}
	}
	if obj.ElseBranch != nil {
		if err := obj.ElseBranch.Apply(fn); err != nil {
			return err
		}
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtIf) Init(data *interfaces.Data) error {
	if err := obj.Condition.Init(data); err != nil {
		return err
	}
	if obj.ThenBranch != nil {
		if err := obj.ThenBranch.Init(data); err != nil {
			return err
		}
	}
	if obj.ElseBranch != nil {
		if err := obj.ElseBranch.Init(data); err != nil {
			return err
		}
	}
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *StmtIf) Interpolate() (interfaces.Stmt, error) {
	condition, err := obj.Condition.Interpolate()
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not interpolate Condition")
	}
	var thenBranch interfaces.Stmt
	if obj.ThenBranch != nil {
		thenBranch, err = obj.ThenBranch.Interpolate()
		if err != nil {
			return nil, errwrap.Wrapf(err, "could not interpolate ThenBranch")
		}
	}
	var elseBranch interfaces.Stmt
	if obj.ElseBranch != nil {
		elseBranch, err = obj.ElseBranch.Interpolate()
		if err != nil {
			return nil, errwrap.Wrapf(err, "could not interpolate ElseBranch")
		}
	}
	return &StmtIf{
		Condition:  condition,
		ThenBranch: thenBranch,
		ElseBranch: elseBranch,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *StmtIf) SetScope(scope *interfaces.Scope) error {
	if err := obj.Condition.SetScope(scope); err != nil {
		return err
	}
	if obj.ThenBranch != nil {
		if err := obj.ThenBranch.SetScope(scope); err != nil {
			return err
		}
	}
	if obj.ElseBranch != nil {
		if err := obj.ElseBranch.SetScope(scope); err != nil {
			return err
		}
	}
	return nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtIf) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	// conditional expression might have some children invariants to share
	condition, err := obj.Condition.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, condition...)

	// the condition must ultimately be a boolean
	conditionInvar := &unification.EqualsInvariant{
		Expr: obj.Condition,
		Type: types.TypeBool,
	}
	invariants = append(invariants, conditionInvar)

	// recurse into the two branches
	if obj.ThenBranch != nil {
		thenBranch, err := obj.ThenBranch.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, thenBranch...)
	}

	if obj.ElseBranch != nil {
		elseBranch, err := obj.ElseBranch.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, elseBranch...)
	}

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This particular if statement doesn't do anything clever here
// other than adding in both branches of the graph. Since we're functional, this
// shouldn't have any ill effects.
// XXX: is this completely true if we're running technically impure, but safe
// built-in functions on both branches? Can we turn off half of this?
func (obj *StmtIf) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("if")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}

	g, err := obj.Condition.Graph()
	if err != nil {
		return nil, err
	}
	graph.AddGraph(g)

	for _, x := range []interfaces.Stmt{obj.ThenBranch, obj.ElseBranch} {
		if x == nil {
			continue
		}
		g, err := x.Graph()
		if err != nil {
			return nil, err
		}
		graph.AddGraph(g)
	}

	return graph, nil
}

// Output returns the output that this "program" produces. This output is what
// is used to build the output graph. This only exists for statements. The
// analogous function for expressions is Value. Those Value functions might get
// called by this Output function if they are needed to produce the output.
func (obj *StmtIf) Output() (*interfaces.Output, error) {
	b, err := obj.Condition.Value()
	if err != nil {
		return nil, err
	}

	var output *interfaces.Output
	if b.Bool() { // must not panic!
		if obj.ThenBranch != nil { // logically then branch is optional
			output, err = obj.ThenBranch.Output()
		}
	} else {
		if obj.ElseBranch != nil { // else branch is optional
			output, err = obj.ElseBranch.Output()
		}
	}
	if err != nil {
		return nil, err
	}

	resources := []engine.Res{}
	edges := []*interfaces.Edge{}
	if output != nil {
		resources = append(resources, output.Resources...)
		edges = append(edges, output.Edges...)
	}

	return &interfaces.Output{
		Resources: resources,
		Edges:     edges,
	}, nil
}

// StmtProg represents a list of stmt's. This usually occurs at the top-level of
// any program, and often within an if stmt. It also contains the logic so that
// the bind statement's are correctly applied in this scope, and irrespective of
// their order of definition.
type StmtProg struct {
	data *interfaces.Data
	// XXX: should this be copied when we run Interpolate here or elsewhere?
	scope *interfaces.Scope // store for use by imports

	// TODO: should this be a map? if so, how would we sort it to loop it?
	importProgs []*StmtProg // list of child programs after running SetScope
	importFiles []string    // list of files seen during the SetScope import

	Prog []interfaces.Stmt
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtProg) Apply(fn func(interfaces.Node) error) error {
	for _, x := range obj.Prog {
		if err := x.Apply(fn); err != nil {
			return err
		}
	}

	// might as well Apply on these too, to make file collection easier, etc
	for _, x := range obj.importProgs {
		if err := x.Apply(fn); err != nil {
			return err
		}
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtProg) Init(data *interfaces.Data) error {
	obj.data = data
	obj.importProgs = []*StmtProg{}
	obj.importFiles = []string{}
	for _, x := range obj.Prog {
		if err := x.Init(data); err != nil {
			return err
		}
	}
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *StmtProg) Interpolate() (interfaces.Stmt, error) {
	prog := []interfaces.Stmt{}
	for _, x := range obj.Prog {
		interpolated, err := x.Interpolate()
		if err != nil {
			return nil, err
		}
		prog = append(prog, interpolated)
	}
	return &StmtProg{
		data:        obj.data,
		importProgs: obj.importProgs, // TODO: do we even need this here?
		importFiles: obj.importFiles,
		Prog:        prog,
	}, nil
}

// importScope is a helper function called from SetScope. If it can't find a
// particular scope, then it can also run the downloader if it is available.
func (obj *StmtProg) importScope(info *interfaces.ImportData, scope *interfaces.Scope) (*interfaces.Scope, error) {
	if obj.data.Debug {
		obj.data.Logf("import: %s", info.Name)
	}
	// the abs file path that we started actively running SetScope on is:
	// obj.data.Base + obj.data.Metadata.Main
	// but recursive imports mean this is not always the active file...

	if info.IsSystem { // system imports are the exact name, eg "fmt"
		systemScope, err := obj.importSystemScope(info.Alias)
		if err != nil {
			return nil, errwrap.Wrapf(err, "system import of `%s` failed", info.Alias)
		}
		return systemScope, nil
	}

	// graph-based recursion detection
	// TODO: is this suffiently unique, but not incorrectly unique?
	// TODO: do we need to clean uvid for consistency so the compare works?
	uvid := obj.data.Base + ";" + info.Name // unique vertex id
	importVertex := obj.data.Imports        // parent vertex
	if importVertex == nil {
		return nil, fmt.Errorf("programming error: missing import vertex")
	}
	importGraph := importVertex.Graph // existing graph (ptr stored within)
	nextVertex := &pgraph.SelfVertex{ // new vertex (if one doesn't already exist)
		Name:  uvid,        // import name
		Graph: importGraph, // store a reference to ourself
	}
	for _, v := range importGraph.VerticesSorted() { // search for one first
		gv, ok := v.(*pgraph.SelfVertex)
		if !ok { // someone misused the vertex
			return nil, fmt.Errorf("programming error: unexpected vertex type")
		}
		if gv.Name == uvid {
			nextVertex = gv // found the same name (use this instead!)
			// this doesn't necessarily mean a cycle. a dag is okay
			break
		}
	}

	// add an edge
	edge := &pgraph.SimpleEdge{Name: ""} // TODO: name me?
	importGraph.AddEdge(importVertex, nextVertex, edge)
	if _, err := importGraph.TopologicalSort(); err != nil {
		// TODO: print the cycle in a prettier way (with file names?)
		obj.data.Logf("import: not a dag:\n%s", importGraph.Sprint())
		return nil, errwrap.Wrapf(err, "recursive import of: `%s`", info.Name)
	}

	if info.IsLocal {
		// append the relative addition of where the running code is, on
		// to the base path that the metadata file (data) is relative to
		// if the main code file has no additional directory, then it is
		// okay, because Dirname collapses down to the empty string here
		importFilePath := obj.data.Base + util.Dirname(obj.data.Metadata.Main) + info.Path
		if obj.data.Debug {
			obj.data.Logf("import: file: %s", importFilePath)
		}
		// don't do this collection here, it has moved elsewhere...
		//obj.importFiles = append(obj.importFiles, importFilePath) // save for CollectFiles

		localScope, err := obj.importScopeWithInputs(importFilePath, scope, nextVertex)
		if err != nil {
			return nil, errwrap.Wrapf(err, "local import of `%s` failed", info.Name)
		}
		return localScope, nil
	}

	// Now, info.IsLocal is false... we're dealing with a remote import!

	// This takes the current metadata as input so it can use the Path
	// directory to search upwards if we wanted to look in parent paths.
	// Since this is an fqdn import, it must contain a metadata file...
	modulesPath, err := interfaces.FindModulesPath(obj.data.Metadata, obj.data.Base, obj.data.Modules)
	if err != nil {
		return nil, errwrap.Wrapf(err, "module path error")
	}
	importFilePath := modulesPath + info.Path + interfaces.MetadataFilename

	if !RequireStrictModulePath { // look upwards
		modulesPathList, err := interfaces.FindModulesPathList(obj.data.Metadata, obj.data.Base, obj.data.Modules)
		if err != nil {
			return nil, errwrap.Wrapf(err, "module path list error")
		}
		for _, mp := range modulesPathList { // first one to find a file
			x := mp + info.Path + interfaces.MetadataFilename
			if _, err := obj.data.Fs.Stat(x); err == nil {
				// found a valid location, so keep using it!
				modulesPath = mp
				importFilePath = x
				break
			}
		}
		// If we get here, and we didn't find anything, then we use the
		// originally decided, most "precise" location... The reason we
		// do that is if the sysadmin wishes to require all the modules
		// to come from their top-level (or higher-level) directory, it
		// can be done by adding the code there, so that it is found in
		// the above upwards search. Otherwise, we just do what the mod
		// asked for and use the path/ directory if it wants its own...
	}
	if obj.data.Debug {
		obj.data.Logf("import: modules path: %s", modulesPath)
		obj.data.Logf("import: file: %s", importFilePath)
	}
	// don't do this collection here, it has moved elsewhere...
	//obj.importFiles = append(obj.importFiles, importFilePath) // save for CollectFiles

	// invoke the download when a path is missing, if the downloader exists
	// we need to invoke the recursive checker before we run this download!
	// this should cleverly deal with skipping modules that are up-to-date!
	if obj.data.Downloader != nil {
		// run downloader stuff first
		if err := obj.data.Downloader.Get(info, modulesPath); err != nil {
			return nil, errwrap.Wrapf(err, "download of `%s` failed", info.Name)
		}
	}

	// takes the full absolute path to the metadata.yaml file
	remoteScope, err := obj.importScopeWithInputs(importFilePath, scope, nextVertex)
	if err != nil {
		return nil, errwrap.Wrapf(err, "remote import of `%s` failed", info.Name)
	}
	return remoteScope, nil
}

// importSystemScope takes the name of a built-in system scope (eg: "fmt") and
// returns the scope struct for that built-in. This function is slightly less
// trivial than expected, because the scope is built from both native mcl code
// and golang code as well. The native mcl code is compiled in as bindata.
// TODO: can we memoize?
func (obj *StmtProg) importSystemScope(name string) (*interfaces.Scope, error) {
	// this basically loop through the registeredFuncs and includes
	// everything that starts with the name prefix and a period, and then
	// lexes and parses the compiled in code, and adds that on top of the
	// scope. we error if there's a duplicate!

	isEmpty := true // assume empty (which should cause an error)

	funcs := funcs.LookupPrefix(name)
	if len(funcs) > 0 {
		isEmpty = false
	}

	// initial scope, built from core golang code
	scope := &interfaces.Scope{
		// TODO: we could add core API's for variables and classes too!
		//Variables: make(map[string]interfaces.Expr),
		Functions: funcs, // map[string]func() interfaces.Func
		//Classes: make(map[string]interfaces.Stmt),
	}

	// TODO: the obj.data.Fs filesystem handle is unused for now, but might
	// be useful if we ever ship all the specific versions of system modules
	// to the remote machines as well, and we want to load off of it...

	// now add any compiled-in mcl code
	paths := bindata.AssetNames()
	// results are not sorted by default (ascertained by reading the code!)
	sort.Strings(paths)
	newScope := interfaces.EmptyScope()
	// XXX: consider using a virtual `append *` statement to combine these instead.
	for _, p := range paths {
		// we only want code from this prefix
		prefix := CoreDir + name + "/"
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		// we only want code from this directory level, so skip children
		// heuristically, a child mcl file will contain a path separator
		if strings.Contains(p[len(prefix):], "/") {
			continue
		}

		b, err := bindata.Asset(p)
		if err != nil {
			return nil, errwrap.Wrapf(err, "can't read asset: `%s`", p)
		}

		// to combine multiple *.mcl files from the same directory, we
		// lex and parse each one individually, which each produces a
		// scope struct. we then merge the scope structs, while making
		// sure we don't overwrite any values. (this logic is only valid
		// for modules, as top-level code combines the output values
		// instead.)

		reader := bytes.NewReader(b) // wrap the byte stream

		// now run the lexer/parser to do the import
		ast, err := LexParse(reader)
		if err != nil {
			return nil, errwrap.Wrapf(err, "could not generate AST from import `%s`", name)
		}
		if obj.data.Debug {
			obj.data.Logf("behold, the AST: %+v", ast)
		}

		obj.data.Logf("init...")
		// init and validate the structure of the AST
		// some of this might happen *after* interpolate in SetScope or Unify...
		if err := ast.Init(obj.data); err != nil {
			return nil, errwrap.Wrapf(err, "could not init and validate AST")
		}

		obj.data.Logf("interpolating...")
		// interpolate strings and other expansionable nodes in AST
		interpolated, err := ast.Interpolate()
		if err != nil {
			return nil, errwrap.Wrapf(err, "could not interpolate AST from import `%s`", name)
		}

		obj.data.Logf("building scope...")
		// propagate the scope down through the AST...
		// most importantly, we ensure that the child imports will run!
		// we pass in *our* parent scope, which will include the globals
		if err := interpolated.SetScope(scope); err != nil {
			return nil, errwrap.Wrapf(err, "could not set scope from import `%s`", name)
		}

		// is the root of our ast a program?
		prog, ok := interpolated.(*StmtProg)
		if !ok {
			return nil, fmt.Errorf("import `%s` did not return a program", name)
		}

		if prog.scope == nil { // pull out the result
			continue // nothing to do here, continue with the next!
		}

		// check for unwanted top-level elements in this module/scope
		// XXX: add a test case to test for this in our core modules!
		if err := prog.IsModuleUnsafe(); err != nil {
			return nil, errwrap.Wrapf(err, "module contains unused statements")
		}

		if !prog.scope.IsEmpty() {
			isEmpty = false // this module/scope isn't empty
		}

		// save a reference to the prog for future usage in Unify/Graph/Etc...
		// XXX: we don't need to do this if we can combine with Append!
		obj.importProgs = append(obj.importProgs, prog)

		// attempt to merge
		// XXX: test for duplicate var/func/class elements in a test!
		if err := newScope.Merge(prog.scope); err != nil { // errors if something was overwritten
			return nil, errwrap.Wrapf(err, "duplicate scope element(s) in module found")
		}
	}

	if err := scope.Merge(newScope); err != nil { // errors if something was overwritten
		return nil, errwrap.Wrapf(err, "duplicate scope element(s) found")
	}

	// when importing a system scope, we only error if there are zero class,
	// function, or variable statements in the scope. We error in this case,
	// because it is non-sensical to import such a scope.
	if isEmpty {
		return nil, fmt.Errorf("could not find any non-empty scope named: %s", name)
	}

	return scope, nil
}

// importScopeWithInputs returns a local or remote scope from an inputs string.
// The inputs string is the common frontend for a lot of our parsing decisions.
func (obj *StmtProg) importScopeWithInputs(s string, scope *interfaces.Scope, parentVertex *pgraph.SelfVertex) (*interfaces.Scope, error) {
	output, err := parseInput(s, obj.data.Fs)
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not activate an input parser")
	}

	// TODO: rm this old, and incorrect, linear file duplicate checking...
	// recursion detection (i guess following the imports has to be a dag!)
	// run recursion detection by checking for duplicates in the seen files
	// TODO: do the paths need to be cleaned for "../", etc before compare?
	//for _, name := range obj.data.Files { // existing seen files
	//	if util.StrInList(name, output.Files) {
	//		return nil, fmt.Errorf("recursive import of: `%s`", name)
	//	}
	//}

	reader := bytes.NewReader(output.Main)

	// nested logger
	logf := func(format string, v ...interface{}) {
		obj.data.Logf("import: "+format, v...)
	}

	// build new list of files
	files := []string{}
	files = append(files, output.Files...)
	files = append(files, obj.data.Files...)

	// store a reference to the parent metadata
	metadata := output.Metadata
	metadata.Metadata = obj.data.Metadata

	// now run the lexer/parser to do the import
	ast, err := LexParse(reader)
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not generate AST from import")
	}
	if obj.data.Debug {
		logf("behold, the AST: %+v", ast)
	}

	logf("init...")
	// init and validate the structure of the AST
	data := &interfaces.Data{
		Fs:         obj.data.Fs,
		Base:       output.Base, // new base dir (absolute path)
		Files:      files,
		Imports:    parentVertex, // the parent vertex that imported me
		Metadata:   metadata,
		Modules:    obj.data.Modules,
		Downloader: obj.data.Downloader,
		//World: obj.data.World,

		//Prefix: obj.Prefix, // TODO: add a path on?
		Debug: obj.data.Debug,
		Logf:  logf,
	}
	// some of this might happen *after* interpolate in SetScope or Unify...
	if err := ast.Init(data); err != nil {
		return nil, errwrap.Wrapf(err, "could not init and validate AST")
	}

	logf("interpolating...")
	// interpolate strings and other expansionable nodes in AST
	interpolated, err := ast.Interpolate()
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not interpolate AST from import")
	}

	logf("building scope...")
	// propagate the scope down through the AST...
	// most importantly, we ensure that the child imports will run!
	// we pass in *our* parent scope, which will include the globals
	if err := interpolated.SetScope(scope); err != nil {
		return nil, errwrap.Wrapf(err, "could not set scope from import")
	}

	// we DON'T do this here anymore, since Apply() digs into the children!
	//// this nested ast needs to pass the data up into the parent!
	//fileList, err := CollectFiles(interpolated)
	//if err != nil {
	//	return nil, errwrap.Wrapf(err, "could not collect files")
	//}
	//obj.importFiles = append(obj.importFiles, fileList...) // save for CollectFiles

	// is the root of our ast a program?
	prog, ok := interpolated.(*StmtProg)
	if !ok {
		return nil, fmt.Errorf("import did not return a program")
	}

	// check for unwanted top-level elements in this module/scope
	// XXX: add a test case to test for this in our core modules!
	if err := prog.IsModuleUnsafe(); err != nil {
		return nil, errwrap.Wrapf(err, "module contains unused statements")
	}

	// when importing a system scope, we only error if there are zero class,
	// function, or variable statements in the scope. We error in this case,
	// because it is non-sensical to import such a scope.
	if prog.scope.IsEmpty() {
		return nil, fmt.Errorf("could not find any non-empty scope")
	}

	// save a reference to the prog for future usage in Unify/Graph/Etc...
	obj.importProgs = append(obj.importProgs, prog)

	// collecting these here is more elegant (and possibly more efficient!)
	obj.importFiles = append(obj.importFiles, output.Files...) // save for CollectFiles

	return prog.scope, nil
}

// SetScope propagates the scope into its list of statements. It does so
// cleverly by first collecting all bind and func statements and adding those
// into the scope after checking for any collisions. Finally it pushes the new
// scope downwards to all child statements. If we support user defined function
// polymorphism via multiple function definition, then these are built together
// here. This SetScope is the one which follows the import statements. If it
// can't follow one (perhaps it wasn't downloaded yet, and is missing) then it
// leaves some information about these missing imports in the AST and errors, so
// that a subsequent AST traversal (usually via Apply) can collect this detailed
// information to be used by the downloader.
func (obj *StmtProg) SetScope(scope *interfaces.Scope) error {
	newScope := scope.Copy()

	// start by looking for any `import` statements to pull into the scope!
	// this will run child lexing/parsing, interpolation, and scope setting
	imports := make(map[string]struct{})
	aliases := make(map[string]struct{})

	// keep track of new imports, to ensure they don't overwrite each other!
	// this is different from scope shadowing which is allowed in new scopes
	newVariables := make(map[string]string)
	newFunctions := make(map[string]string)
	newClasses := make(map[string]string)
	for _, x := range obj.Prog {
		imp, ok := x.(*StmtImport)
		if !ok {
			continue
		}
		// check for duplicates *in this scope*
		if _, exists := imports[imp.Name]; exists {
			return fmt.Errorf("import `%s` already exists in this scope", imp.Name)
		}

		result, err := ParseImportName(imp.Name)
		if err != nil {
			return errwrap.Wrapf(err, "import `%s` is not valid", imp.Name)
		}
		alias := result.Alias // this is what we normally call the import

		if imp.Alias != "" { // this is what the user decided as the name
			alias = imp.Alias // use alias if specified
		}
		if _, exists := aliases[alias]; exists {
			return fmt.Errorf("import alias `%s` already exists in this scope", alias)
		}

		// run the scope importer...
		importedScope, err := obj.importScope(result, scope)
		if err != nil {
			return errwrap.Wrapf(err, "import scope `%s` failed", imp.Name)
		}

		// read from stored scope which was previously saved in SetScope
		// add to scope, (overwriting, aka shadowing is ok)
		// rename scope values, adding the alias prefix
		// check that we don't overwrite a new value from another import
		// TODO: do this in a deterministic (sorted) order
		for name, x := range importedScope.Variables {
			newName := alias + interfaces.ModuleSep + name
			if alias == "*" {
				newName = name
			}
			if previous, exists := newVariables[newName]; exists {
				// don't overwrite in same scope
				return fmt.Errorf("can't squash variable `%s` from `%s` by import of `%s`", newName, previous, imp.Name)
			}
			newVariables[newName] = imp.Name
			newScope.Variables[newName] = x // merge
		}
		for name, x := range importedScope.Functions {
			newName := alias + interfaces.ModuleSep + name
			if alias == "*" {
				newName = name
			}
			if previous, exists := newFunctions[newName]; exists {
				// don't overwrite in same scope
				return fmt.Errorf("can't squash function `%s` from `%s` by import of `%s`", newName, previous, imp.Name)
			}
			newFunctions[newName] = imp.Name
			newScope.Functions[newName] = x
		}
		for name, x := range importedScope.Classes {
			newName := alias + interfaces.ModuleSep + name
			if alias == "*" {
				newName = name
			}
			if previous, exists := newClasses[newName]; exists {
				// don't overwrite in same scope
				return fmt.Errorf("can't squash class `%s` from `%s` by import of `%s`", newName, previous, imp.Name)
			}
			newClasses[newName] = imp.Name
			newScope.Classes[newName] = x
		}

		// everything has been merged, move on to next import...
		imports[imp.Name] = struct{}{} // mark as found in scope
		aliases[alias] = struct{}{}
	}

	// collect all the bind statements in the first pass
	// this allows them to appear out of order in this scope
	binds := make(map[string]struct{}) // bind existence in this scope
	for _, x := range obj.Prog {
		bind, ok := x.(*StmtBind)
		if !ok {
			continue
		}
		// check for duplicates *in this scope*
		if _, exists := binds[bind.Ident]; exists {
			return fmt.Errorf("var `%s` already exists in this scope", bind.Ident)
		}

		binds[bind.Ident] = struct{}{} // mark as found in scope
		// add to scope, (overwriting, aka shadowing is ok)
		newScope.Variables[bind.Ident] = bind.Value
	}

	// now collect all the functions, and group by name (if polyfunc is ok)
	funcs := make(map[string][]*StmtFunc)
	for _, x := range obj.Prog {
		fn, ok := x.(*StmtFunc)
		if !ok {
			continue
		}

		_, exists := funcs[fn.Name]
		if !exists {
			funcs[fn.Name] = []*StmtFunc{} // initialize
		}

		// check for duplicates *in this scope*
		if exists && !AllowUserDefinedPolyFunc {
			return fmt.Errorf("func `%s` already exists in this scope", fn.Name)
		}

		// collect funcs (if multiple, this is a polyfunc)
		funcs[fn.Name] = append(funcs[fn.Name], fn)
	}

	for name, fnList := range funcs {
		// add to scope, (overwriting, aka shadowing is ok)
		if len(fnList) == 1 {
			fn := fnList[0].Func // local reference to avoid changing it in the loop...
			f, err := fn.Func()
			if err != nil {
				return errwrap.Wrapf(err, "could not build func from: %s", fnList[0].Name)
			}
			newScope.Functions[name] = func() interfaces.Func { return f }
			continue
		}

		// build polyfunc's
		// XXX: not implemented
	}

	// now collect any classes
	// TODO: if we ever allow poly classes, then group in lists by name
	classes := make(map[string]struct{})
	for _, x := range obj.Prog {
		class, ok := x.(*StmtClass)
		if !ok {
			continue
		}
		// check for duplicates *in this scope*
		if _, exists := classes[class.Name]; exists {
			return fmt.Errorf("class `%s` already exists in this scope", class.Name)
		}

		classes[class.Name] = struct{}{} // mark as found in scope
		// add to scope, (overwriting, aka shadowing is ok)
		newScope.Classes[class.Name] = class
	}

	obj.scope = newScope // save a reference in case we're read by an import

	// now set the child scopes (even on bind...)
	for _, x := range obj.Prog {
		// skip over *StmtClass here (essential for recursive classes)
		if _, ok := x.(*StmtClass); ok {
			continue
		}

		if err := x.SetScope(newScope); err != nil {
			return err
		}
	}

	return nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtProg) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	// collect all the invariants of each sub-expression
	for _, x := range obj.Prog {
		// skip over *StmtClass here
		if _, ok := x.(*StmtClass); ok {
			continue
		}

		invars, err := x.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invars...)
	}

	// add invariants from SetScope's imported child programs
	for _, x := range obj.importProgs {
		invars, err := x.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invars...)
	}

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might.
func (obj *StmtProg) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("prog")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}

	// collect all graphs that need to be included
	for _, x := range obj.Prog {
		// skip over *StmtClass here
		if _, ok := x.(*StmtClass); ok {
			continue
		}

		g, err := x.Graph()
		if err != nil {
			return nil, err
		}
		graph.AddGraph(g)
	}

	// add graphs from SetScope's imported child programs
	for _, x := range obj.importProgs {
		g, err := x.Graph()
		if err != nil {
			return nil, err
		}
		graph.AddGraph(g)
	}

	return graph, nil
}

// Output returns the output that this "program" produces. This output is what
// is used to build the output graph. This only exists for statements. The
// analogous function for expressions is Value. Those Value functions might get
// called by this Output function if they are needed to produce the output.
func (obj *StmtProg) Output() (*interfaces.Output, error) {
	resources := []engine.Res{}
	edges := []*interfaces.Edge{}

	for _, stmt := range obj.Prog {
		// skip over *StmtClass here so its Output method can be used...
		if _, ok := stmt.(*StmtClass); ok {
			// don't read output from StmtClass, it
			// gets consumed by StmtInclude instead
			continue
		}

		output, err := stmt.Output()
		if err != nil {
			return nil, err
		}
		if output != nil {
			resources = append(resources, output.Resources...)
			edges = append(edges, output.Edges...)
		}
	}

	// nothing to add from SetScope's imported child programs

	return &interfaces.Output{
		Resources: resources,
		Edges:     edges,
	}, nil
}

// IsModuleUnsafe returns whether or not this StmtProg is unsafe to consume as a
// module scope. IOW, if someone writes a module which is imported and which has
// statements other than bind, func, class or import, then it is not correct to
// import, since those other elements wouldn't be used, and might provide a
// false belief that they'll get included when mgmt imports that module.
// SetScope should be called before this is used. (TODO: verify this)
// TODO: return a multierr with all the unsafe elements, to provide better info
// TODO: technically this could be a method on Stmt, possibly using Apply...
func (obj *StmtProg) IsModuleUnsafe() error { // TODO: rename this function?
	for _, x := range obj.Prog {
		// stmt's allowed: import, bind, func, class
		// stmt's not-allowed: if, include, res, edge
		switch x.(type) {
		case *StmtImport:
		case *StmtBind:
		case *StmtFunc:
		case *StmtClass:
		case *StmtComment: // possibly not even parsed
			// all of these are safe
		default:
			// something else unsafe
			return fmt.Errorf("found unsafe stmt: %v", x)
		}
	}
	return nil
}

// StmtFunc represents a user defined function. It binds the specified name to
// the supplied function in the current scope and irrespective of the order of
// definition.
type StmtFunc struct {
	Name string
	//Func *ExprFunc // TODO: should it be this instead?
	Func interfaces.Expr // TODO: is this correct?
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtFunc) Apply(fn func(interfaces.Node) error) error {
	if err := obj.Func.Apply(fn); err != nil {
		return err
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtFunc) Init(data *interfaces.Data) error {
	//obj.data = data // TODO: ???
	if err := obj.Func.Init(data); err != nil {
		return err
	}
	return nil
}

// Interpolate returns a new node (or itself) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *StmtFunc) Interpolate() (interfaces.Stmt, error) {
	interpolated, err := obj.Func.Interpolate()
	if err != nil {
		return nil, err
	}

	return &StmtFunc{
		Name: obj.Name,
		Func: interpolated,
	}, nil
}

// SetScope sets the scope of the child expression bound to it. It seems this is
// necessary in order to reach this, in particular in situations when a bound
// expression points to a previously bound expression.
func (obj *StmtFunc) SetScope(scope *interfaces.Scope) error {
	return obj.Func.SetScope(scope)
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtFunc) Unify() ([]interfaces.Invariant, error) {
	if obj.Name == "" {
		return nil, fmt.Errorf("missing function name")
	}
	return obj.Func.Unify()
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This particular func statement adds its linked expression to
// the graph.
func (obj *StmtFunc) Graph() (*pgraph.Graph, error) {
	return obj.Func.Graph()
}

// Output for the func statement produces no output. Any values of interest come
// from the use of the func which this binds the function to.
func (obj *StmtFunc) Output() (*interfaces.Output, error) {
	return interfaces.EmptyOutput(), nil
}

// StmtClass represents a user defined class. It's effectively a program body
// that can optionally take some parameterized inputs.
// TODO: We don't currently support defining polymorphic classes (eg: different
// signatures for the same class name) but it might be something to consider.
type StmtClass struct {
	Name string
	Args []*Arg
	Body interfaces.Stmt // probably a *StmtProg
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtClass) Apply(fn func(interfaces.Node) error) error {
	if err := obj.Body.Apply(fn); err != nil {
		return err
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtClass) Init(data *interfaces.Data) error {
	return obj.Body.Init(data)
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *StmtClass) Interpolate() (interfaces.Stmt, error) {
	interpolated, err := obj.Body.Interpolate()
	if err != nil {
		return nil, err
	}

	args := obj.Args
	if obj.Args == nil {
		args = []*Arg{}
	}

	return &StmtClass{
		Name: obj.Name,
		Args: args, // ensure this has length == 0 instead of nil
		Body: interpolated,
	}, nil
}

// SetScope sets the scope of the child expression bound to it. It seems this is
// necessary in order to reach this, in particular in situations when a bound
// expression points to a previously bound expression.
func (obj *StmtClass) SetScope(scope *interfaces.Scope) error {
	return obj.Body.SetScope(scope)
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtClass) Unify() ([]interfaces.Invariant, error) {
	if obj.Name == "" {
		return nil, fmt.Errorf("missing class name")
	}

	// TODO: do we need to add anything else here because of the obj.Args ?
	return obj.Body.Unify()
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This particular func statement adds its linked expression to
// the graph.
func (obj *StmtClass) Graph() (*pgraph.Graph, error) {
	return obj.Body.Graph()
}

// Output for the class statement produces no output. Any values of interest
// come from the use of the include which this binds the statements to. This is
// usually called from the parent in StmtProg, but it skips running it so that
// it can be called from the StmtInclude Output method.
func (obj *StmtClass) Output() (*interfaces.Output, error) {
	return obj.Body.Output()
}

// StmtInclude causes a user defined class to get used. It's effectively the way
// to call a class except that it produces output instead of a value. Most of
// the interesting logic for classes happens here or in StmtProg.
type StmtInclude struct {
	class *StmtClass   // copy of class that we're using
	orig  *StmtInclude // original pointer to this

	Name string
	Args []interfaces.Expr
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtInclude) Apply(fn func(interfaces.Node) error) error {
	if obj.Args != nil {
		for _, x := range obj.Args {
			if err := x.Apply(fn); err != nil {
				return err
			}
		}
	}
	return fn(obj)
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtInclude) Init(data *interfaces.Data) error {
	if obj.Args != nil {
		for _, x := range obj.Args {
			if err := x.Init(data); err != nil {
				return err
			}
		}
	}
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *StmtInclude) Interpolate() (interfaces.Stmt, error) {
	args := []interfaces.Expr{}
	if obj.Args != nil {
		for _, x := range obj.Args {
			interpolated, err := x.Interpolate()
			if err != nil {
				return nil, err
			}
			args = append(args, interpolated)
		}
	}

	orig := obj
	if obj.orig != nil { // preserve the original pointer (the identifier!)
		orig = obj.orig
	}
	return &StmtInclude{
		orig: orig,
		Name: obj.Name,
		Args: args,
	}, nil
}

// SetScope stores the scope for use in this statement. Since this is the first
// location where recursion would play an important role, this also detects and
// handles the recursion scenario.
func (obj *StmtInclude) SetScope(scope *interfaces.Scope) error {
	if scope == nil {
		scope = interfaces.EmptyScope()
	}

	stmt, exists := scope.Classes[obj.Name]
	if !exists {
		return fmt.Errorf("class `%s` does not exist in this scope", obj.Name)
	}
	class, ok := stmt.(*StmtClass)
	if !ok {
		return fmt.Errorf("class scope of `%s` does not contain a class", obj.Name)
	}

	// is it even possible for the signatures to match?
	if len(class.Args) != len(obj.Args) {
		return fmt.Errorf("class `%s` expected %d args but got %d", obj.Name, len(class.Args), len(obj.Args))
	}

	if obj.class != nil {
		// possible programming error
		return fmt.Errorf("include already contains a class pointer")
	}

	for i := len(scope.Chain) - 1; i >= 0; i-- { // reverse order
		x, ok := scope.Chain[i].(*StmtInclude)
		if !ok {
			continue
		}

		if x == obj.orig { // look for my original self
			// scope chain found!
			obj.class = class // same pointer, don't copy
			return fmt.Errorf("recursive class `%s` found", obj.Name)
			//return nil // if recursion was supported
		}
	}

	// helper function to keep things more logical
	cp := func(input *StmtClass) (*StmtClass, error) {
		// TODO: should we have a dedicated copy method instead? because
		// we want to copy some things, but not others like Expr I think
		copied, err := input.Interpolate() // this sort of copies things
		if err != nil {
			return nil, errwrap.Wrapf(err, "could not copy class")
		}
		class, ok := copied.(*StmtClass) // convert it back again
		if !ok {
			return nil, fmt.Errorf("copied class named `%s` is not a class", obj.Name)
		}
		return class, nil
	}

	copied, err := cp(class) // copy it for each use of the include
	if err != nil {
		return errwrap.Wrapf(err, "could not copy class")
	}
	obj.class = copied

	newScope := scope.Copy()
	for i, arg := range obj.class.Args { // copy
		newScope.Variables[arg.Name] = obj.Args[i]
	}

	// recursion detection
	newScope.Chain = append(newScope.Chain, obj.orig) // add stmt to list
	newScope.Classes[obj.Name] = copied               // overwrite with new pointer

	if err := obj.class.SetScope(newScope); err != nil {
		return err
	}

	return nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtInclude) Unify() ([]interfaces.Invariant, error) {
	if obj.Name == "" {
		return nil, fmt.Errorf("missing include name")
	}

	// is it even possible for the signatures to match?
	if len(obj.class.Args) != len(obj.Args) {
		return nil, fmt.Errorf("class `%s` expected %d args but got %d", obj.Name, len(obj.class.Args), len(obj.Args))
	}

	var invariants []interfaces.Invariant

	// do this here because we skip doing it in the StmtProg parent
	invars, err := obj.class.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, invars...)

	// collect all the invariants of each sub-expression
	for i, x := range obj.Args {
		invars, err := x.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invars...)

		// TODO: are additional invariants required?
		// add invariants between the args and the class
		if typ := obj.class.Args[i].Type; typ != nil {
			invar := &unification.EqualsInvariant{
				Expr: obj.Args[i],
				Type: typ, // type of arg
			}
			invariants = append(invariants, invar)
		}
	}

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This particular func statement adds its linked expression to
// the graph.
func (obj *StmtInclude) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("include")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}

	g, err := obj.class.Graph()
	if err != nil {
		return nil, err
	}
	graph.AddGraph(g)

	return graph, nil
}

// Output returns the output that this include produces. This output is what
// is used to build the output graph. This only exists for statements. The
// analogous function for expressions is Value. Those Value functions might get
// called by this Output function if they are needed to produce the output. The
// ultimate source of this output comes from the previously defined StmtClass
// which should be found in our scope.
func (obj *StmtInclude) Output() (*interfaces.Output, error) {
	return obj.class.Output()
}

// StmtImport adds the exported scope definitions of a module into the current
// scope. It can be used anywhere a statement is allowed, and can even be nested
// inside a class definition. By convention, it is commonly used at the top of a
// file. As with any statement, it produces output, but that output is empty. To
// benefit from its inclusion, reference the scope definitions you want.
type StmtImport struct {
	Name  string
	Alias string
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtImport) Apply(fn func(interfaces.Node) error) error { return fn(obj) }

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtImport) Init(*interfaces.Data) error { return nil }

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *StmtImport) Interpolate() (interfaces.Stmt, error) {
	return &StmtImport{
		Name:  obj.Name,
		Alias: obj.Alias,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *StmtImport) SetScope(*interfaces.Scope) error { return nil }

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtImport) Unify() ([]interfaces.Invariant, error) {
	if obj.Name == "" {
		return nil, fmt.Errorf("missing import name")
	}

	return []interfaces.Invariant{}, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This particular statement just returns an empty graph.
func (obj *StmtImport) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("import")
	return graph, errwrap.Wrapf(err, "could not create graph")
}

// Output returns the output that this include produces. This output is what
// is used to build the output graph. This only exists for statements. The
// analogous function for expressions is Value. Those Value functions might get
// called by this Output function if they are needed to produce the output. This
// import statement itself produces no output, as it is only used to populate
// the scope so that others can use that to produce values and output.
func (obj *StmtImport) Output() (*interfaces.Output, error) {
	return interfaces.EmptyOutput(), nil
}

// StmtComment is a representation of a comment. It is currently unused. It
// probably makes sense to make a third kind of Node (not a Stmt or an Expr) so
// that comments can still be part of the AST (for eventual automatic code
// formatting) but so that they can exist anywhere in the code. Currently these
// are dropped by the lexer.
type StmtComment struct {
	Value string
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *StmtComment) Apply(fn func(interfaces.Node) error) error { return fn(obj) }

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *StmtComment) Init(*interfaces.Data) error {
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// Here it simply returns itself, as no interpolation is possible.
func (obj *StmtComment) Interpolate() (interfaces.Stmt, error) {
	return &StmtComment{
		Value: obj.Value,
	}, nil
}

// SetScope does nothing for this struct, because it has no child nodes, and it
// does not need to know about the parent scope.
func (obj *StmtComment) SetScope(*interfaces.Scope) error { return nil }

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *StmtComment) Unify() ([]interfaces.Invariant, error) {
	return []interfaces.Invariant{}, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This particular graph does nothing clever.
func (obj *StmtComment) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("comment")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	return graph, nil
}

// Output for the comment statement produces no output.
func (obj *StmtComment) Output() (*interfaces.Output, error) {
	return interfaces.EmptyOutput(), nil
}

// ExprAny is a placeholder expression that is used for type unification hacks.
type ExprAny struct {
	typ *types.Type
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprAny) Apply(fn func(interfaces.Node) error) error { return fn(obj) }

// String returns a short representation of this expression.
func (obj *ExprAny) String() string { return "any" }

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprAny) Init(*interfaces.Data) error { return nil }

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// Here it simply returns itself, as no interpolation is possible.
func (obj *ExprAny) Interpolate() (interfaces.Expr, error) {
	return &ExprAny{
		typ: obj.typ,
	}, nil
}

// SetScope does nothing for this struct, because it has no child nodes, and it
// does not need to know about the parent scope.
func (obj *ExprAny) SetScope(*interfaces.Scope) error { return nil }

// SetType is used to set the type of this expression once it is known. This
// usually happens during type unification, but it can also happen during
// parsing if a type is specified explicitly. Since types are static and don't
// change on expressions, if you attempt to set a different type than what has
// previously been set (when not initially known) this will error.
func (obj *ExprAny) SetType(typ *types.Type) error {
	if obj.typ != nil {
		return obj.typ.Cmp(typ) // if not set, ensure it doesn't change
	}
	obj.typ = typ // set
	return nil
}

// Type returns the type of this expression.
func (obj *ExprAny) Type() (*types.Type, error) {
	if obj.typ == nil {
		return nil, interfaces.ErrTypeCurrentlyUnknown
	}
	return obj.typ, nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprAny) Unify() ([]interfaces.Invariant, error) {
	invariants := []interfaces.Invariant{
		&unification.AnyInvariant{ // it has to be something, anything!
			Expr: obj,
		},
	}
	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it, and
// the edges from all of the child graphs to this.
func (obj *ExprAny) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("any")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)
	return graph, nil
}

// Func returns the reactive stream of values that this expression produces.
func (obj *ExprAny) Func() (interfaces.Func, error) {
	return nil, fmt.Errorf("programming error") // this should not be called
}

// SetValue here is a no-op, because algorithmically when this is called from
// the func engine, the child elements (the list elements) will have had this
// done to them first, and as such when we try and retrieve the set value from
// this expression by calling `Value`, it will build it from scratch!
func (obj *ExprAny) SetValue(value types.Value) error {
	return fmt.Errorf("programming error") // this should not be called
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
func (obj *ExprAny) Value() (types.Value, error) {
	return nil, fmt.Errorf("programming error") // this should not be called
}

// ExprBool is a representation of a boolean.
type ExprBool struct {
	V bool
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprBool) Apply(fn func(interfaces.Node) error) error { return fn(obj) }

// String returns a short representation of this expression.
func (obj *ExprBool) String() string { return fmt.Sprintf("bool(%t)", obj.V) }

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprBool) Init(*interfaces.Data) error { return nil }

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// Here it simply returns itself, as no interpolation is possible.
func (obj *ExprBool) Interpolate() (interfaces.Expr, error) {
	return &ExprBool{
		V: obj.V,
	}, nil
}

// SetScope does nothing for this struct, because it has no child nodes, and it
// does not need to know about the parent scope.
func (obj *ExprBool) SetScope(*interfaces.Scope) error { return nil }

// SetType will make no changes if called here. It will error if anything other
// than a Bool is passed in, and doesn't need to be called for this expr to work.
func (obj *ExprBool) SetType(typ *types.Type) error { return types.TypeBool.Cmp(typ) }

// Type returns the type of this expression. This method always returns Bool here.
func (obj *ExprBool) Type() (*types.Type, error) { return types.TypeBool, nil }

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprBool) Unify() ([]interfaces.Invariant, error) {
	invariants := []interfaces.Invariant{
		&unification.EqualsInvariant{
			Expr: obj,
			Type: types.TypeBool,
		},
	}
	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it.
func (obj *ExprBool) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("bool")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)
	return graph, nil
}

// Func returns the reactive stream of values that this expression produces.
func (obj *ExprBool) Func() (interfaces.Func, error) {
	return &structs.ConstFunc{
		Value: &types.BoolValue{V: obj.V},
	}, nil
}

// SetValue for a bool expression is always populated statically, and does not
// ever receive any incoming values (no incoming edges) so this should never be
// called. It has been implemented for uniformity.
func (obj *ExprBool) SetValue(value types.Value) error {
	if err := types.TypeBool.Cmp(value.Type()); err != nil {
		return err
	}
	obj.V = value.Bool()
	return nil
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
// This particular value is always known since it is a constant.
func (obj *ExprBool) Value() (types.Value, error) {
	return &types.BoolValue{
		V: obj.V,
	}, nil
}

// ExprStr is a representation of a string.
type ExprStr struct {
	data *interfaces.Data

	V string // value of this string
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprStr) Apply(fn func(interfaces.Node) error) error { return fn(obj) }

// String returns a short representation of this expression.
func (obj *ExprStr) String() string { return fmt.Sprintf("str(%s)", obj.V) }

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprStr) Init(data *interfaces.Data) error {
	obj.data = data
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// Here it attempts to expand the string if there are any internal variables
// which need interpolation. If any are found, it returns a larger AST which
// has a function which returns a string as its root. Otherwise it returns
// itself.
func (obj *ExprStr) Interpolate() (interfaces.Expr, error) {
	pos := &Pos{
		// column/line number, starting at 1
		//Column: -1, // TODO
		//Line: -1, // TODO
		//Filename: "", // optional source filename, if known
	}
	info := &InterpolateInfo{
		Debug: obj.data.Debug,
		Logf: func(format string, v ...interface{}) {
			obj.data.Logf("interpolate: "+format, v...)
		},
	}
	result, err := InterpolateStr(obj.V, pos, info)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &ExprStr{
			data: obj.data,
			V:    obj.V,
		}, nil
	}
	// we got something, overwrite the existing static str
	return result, nil // replacement
}

// SetScope does nothing for this struct, because it has no child nodes, and it
// does not need to know about the parent scope.
func (obj *ExprStr) SetScope(*interfaces.Scope) error { return nil }

// SetType will make no changes if called here. It will error if anything other
// than an Str is passed in, and doesn't need to be called for this expr to work.
func (obj *ExprStr) SetType(typ *types.Type) error { return types.TypeStr.Cmp(typ) }

// Type returns the type of this expression. This method always returns Str here.
func (obj *ExprStr) Type() (*types.Type, error) { return types.TypeStr, nil }

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprStr) Unify() ([]interfaces.Invariant, error) {
	invariants := []interfaces.Invariant{
		&unification.EqualsInvariant{
			Expr: obj, // unique id for this expression (a pointer)
			Type: types.TypeStr,
		},
	}
	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it.
func (obj *ExprStr) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("str")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)
	return graph, nil
}

// Func returns the reactive stream of values that this expression produces.
func (obj *ExprStr) Func() (interfaces.Func, error) {
	return &structs.ConstFunc{
		Value: &types.StrValue{V: obj.V},
	}, nil
}

// SetValue for an str expression is always populated statically, and does not
// ever receive any incoming values (no incoming edges) so this should never be
// called. It has been implemented for uniformity.
func (obj *ExprStr) SetValue(value types.Value) error {
	if err := types.TypeStr.Cmp(value.Type()); err != nil {
		return err
	}
	obj.V = value.Str()
	return nil
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
// This particular value is always known since it is a constant.
func (obj *ExprStr) Value() (types.Value, error) {
	return &types.StrValue{
		V: obj.V,
	}, nil
}

// ExprInt is a representation of an int.
type ExprInt struct {
	V int64
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprInt) Apply(fn func(interfaces.Node) error) error { return fn(obj) }

// String returns a short representation of this expression.
func (obj *ExprInt) String() string { return fmt.Sprintf("int(%d)", obj.V) }

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprInt) Init(*interfaces.Data) error { return nil }

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// Here it simply returns itself, as no interpolation is possible.
func (obj *ExprInt) Interpolate() (interfaces.Expr, error) {
	return &ExprInt{
		V: obj.V,
	}, nil
}

// SetScope does nothing for this struct, because it has no child nodes, and it
// does not need to know about the parent scope.
func (obj *ExprInt) SetScope(*interfaces.Scope) error { return nil }

// SetType will make no changes if called here. It will error if anything other
// than an Int is passed in, and doesn't need to be called for this expr to work.
func (obj *ExprInt) SetType(typ *types.Type) error { return types.TypeInt.Cmp(typ) }

// Type returns the type of this expression. This method always returns Int here.
func (obj *ExprInt) Type() (*types.Type, error) { return types.TypeInt, nil }

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprInt) Unify() ([]interfaces.Invariant, error) {
	invariants := []interfaces.Invariant{
		&unification.EqualsInvariant{
			Expr: obj,
			Type: types.TypeInt,
		},
	}
	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it.
func (obj *ExprInt) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("int")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)
	return graph, nil
}

// Func returns the reactive stream of values that this expression produces.
func (obj *ExprInt) Func() (interfaces.Func, error) {
	return &structs.ConstFunc{
		Value: &types.IntValue{V: obj.V},
	}, nil
}

// SetValue for an int expression is always populated statically, and does not
// ever receive any incoming values (no incoming edges) so this should never be
// called. It has been implemented for uniformity.
func (obj *ExprInt) SetValue(value types.Value) error {
	if err := types.TypeInt.Cmp(value.Type()); err != nil {
		return err
	}
	obj.V = value.Int()
	return nil
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
// This particular value is always known since it is a constant.
func (obj *ExprInt) Value() (types.Value, error) {
	return &types.IntValue{
		V: obj.V,
	}, nil
}

// ExprFloat is a representation of a float.
type ExprFloat struct {
	V float64
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprFloat) Apply(fn func(interfaces.Node) error) error { return fn(obj) }

// String returns a short representation of this expression.
func (obj *ExprFloat) String() string {
	return fmt.Sprintf("float(%g)", obj.V) // TODO: %f instead?
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprFloat) Init(*interfaces.Data) error { return nil }

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// Here it simply returns itself, as no interpolation is possible.
func (obj *ExprFloat) Interpolate() (interfaces.Expr, error) {
	return &ExprFloat{
		V: obj.V,
	}, nil
}

// SetScope does nothing for this struct, because it has no child nodes, and it
// does not need to know about the parent scope.
func (obj *ExprFloat) SetScope(*interfaces.Scope) error { return nil }

// SetType will make no changes if called here. It will error if anything other
// than a Float is passed in, and doesn't need to be called for this expr to work.
func (obj *ExprFloat) SetType(typ *types.Type) error { return types.TypeFloat.Cmp(typ) }

// Type returns the type of this expression. This method always returns Float here.
func (obj *ExprFloat) Type() (*types.Type, error) { return types.TypeFloat, nil }

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprFloat) Unify() ([]interfaces.Invariant, error) {
	invariants := []interfaces.Invariant{
		&unification.EqualsInvariant{
			Expr: obj,
			Type: types.TypeFloat,
		},
	}
	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it.
func (obj *ExprFloat) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("float")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)
	return graph, nil
}

// Func returns the reactive stream of values that this expression produces.
func (obj *ExprFloat) Func() (interfaces.Func, error) {
	return &structs.ConstFunc{
		Value: &types.FloatValue{V: obj.V},
	}, nil
}

// SetValue for a float expression is always populated statically, and does not
// ever receive any incoming values (no incoming edges) so this should never be
// called. It has been implemented for uniformity.
func (obj *ExprFloat) SetValue(value types.Value) error {
	if err := types.TypeFloat.Cmp(value.Type()); err != nil {
		return err
	}
	obj.V = value.Float()
	return nil
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
// This particular value is always known since it is a constant.
func (obj *ExprFloat) Value() (types.Value, error) {
	return &types.FloatValue{
		V: obj.V,
	}, nil
}

// ExprList is a representation of a list.
type ExprList struct {
	typ *types.Type

	//Elements []*ExprListElement
	Elements []interfaces.Expr
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprList) Apply(fn func(interfaces.Node) error) error {
	for _, x := range obj.Elements {
		if err := x.Apply(fn); err != nil {
			return err
		}
	}
	return fn(obj)
}

// String returns a short representation of this expression.
func (obj *ExprList) String() string {
	var s []string
	for _, x := range obj.Elements {
		s = append(s, x.String())
	}
	return fmt.Sprintf("list(%s)", strings.Join(s, ", "))
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprList) Init(data *interfaces.Data) error {
	for _, x := range obj.Elements {
		if err := x.Init(data); err != nil {
			return err
		}
	}
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *ExprList) Interpolate() (interfaces.Expr, error) {
	elements := []interfaces.Expr{}
	for _, x := range obj.Elements {
		interpolated, err := x.Interpolate()
		if err != nil {
			return nil, err
		}
		elements = append(elements, interpolated)
	}
	return &ExprList{
		typ:      obj.typ,
		Elements: elements,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *ExprList) SetScope(scope *interfaces.Scope) error {
	for _, x := range obj.Elements {
		if err := x.SetScope(scope); err != nil {
			return err
		}
	}
	return nil
}

// SetType is used to set the type of this expression once it is known. This
// usually happens during type unification, but it can also happen during
// parsing if a type is specified explicitly. Since types are static and don't
// change on expressions, if you attempt to set a different type than what has
// previously been set (when not initially known) this will error.
func (obj *ExprList) SetType(typ *types.Type) error {
	// TODO: should we ensure this is set to a KindList ?
	if obj.typ != nil {
		return obj.typ.Cmp(typ) // if not set, ensure it doesn't change
	}
	obj.typ = typ // set
	return nil
}

// Type returns the type of this expression.
func (obj *ExprList) Type() (*types.Type, error) {
	var typ *types.Type
	var err error
	for i, expr := range obj.Elements {
		etyp, e := expr.Type()
		if e != nil {
			err = errwrap.Wrapf(e, "list index `%d` did not return a type", i)
			break
		}
		if typ == nil {
			typ = etyp
		}
		if e := typ.Cmp(etyp); e != nil {
			err = errwrap.Wrapf(e, "list elements have different types")
			break
		}
	}
	if err == nil && obj.typ == nil && len(obj.Elements) > 0 {
		return &types.Type{ // speculate!
			Kind: types.KindList,
			Val:  typ,
		}, nil
	}

	if obj.typ == nil {
		return nil, interfaces.ErrTypeCurrentlyUnknown
	}
	return obj.typ, nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprList) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	// if this was set explicitly by the parser
	if obj.typ != nil {
		invar := &unification.EqualsInvariant{
			Expr: obj,
			Type: obj.typ,
		}
		invariants = append(invariants, invar)
	}

	// collect all the invariants of each sub-expression
	for _, x := range obj.Elements {
		invars, err := x.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invars...)
	}

	// each element must be equal to each other
	if len(obj.Elements) > 1 {
		invariant := &unification.EqualityInvariantList{
			Exprs: obj.Elements,
		}
		invariants = append(invariants, invariant)
	}

	// we should be type list of (type of element)
	if len(obj.Elements) > 0 {
		invariant := &unification.EqualityWrapListInvariant{
			Expr1:    obj, // unique id for this expression (a pointer)
			Expr2Val: obj.Elements[0],
		}
		invariants = append(invariants, invariant)
	}

	// make sure this empty list gets an element type somehow
	if len(obj.Elements) == 0 {
		invariant := &unification.AnyInvariant{
			Expr: obj,
		}
		invariants = append(invariants, invariant)

		// build a placeholder expr to represent a contained element...
		exprAny := &ExprAny{}
		invars, err := exprAny.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invars...)

		// FIXME: instead of using `ExprAny`, we could actually teach
		// our unification engine to ensure that our expr kind is list,
		// eg:
		//&unification.EqualityKindInvariant{
		//	Expr1: obj,
		//	Kind:  types.KindList,
		//}
		invar := &unification.EqualityWrapListInvariant{
			Expr1:    obj,
			Expr2Val: exprAny, // hack
		}
		invariants = append(invariants, invar)
	}

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it, and
// the edges from all of the child graphs to this.
func (obj *ExprList) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("list")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)

	// each list element needs to point to the final list expression
	for index, x := range obj.Elements { // list elements in order
		g, err := x.Graph()
		if err != nil {
			return nil, err
		}

		fieldName := fmt.Sprintf("%d", index) // argNames as integers!
		edge := &funcs.Edge{Args: []string{fieldName}}

		var once bool
		edgeGenFn := func(v1, v2 pgraph.Vertex) pgraph.Edge {
			if once {
				panic(fmt.Sprintf("edgeGenFn for list, index `%d` was called twice", index))
			}
			once = true
			return edge
		}
		graph.AddEdgeGraphVertexLight(g, obj, edgeGenFn) // element -> list
	}

	return graph, nil
}

// Func returns the reactive stream of values that this expression produces.
func (obj *ExprList) Func() (interfaces.Func, error) {
	typ, err := obj.Type()
	if err != nil {
		return nil, err
	}

	// composite func (list, map, struct)
	return &structs.CompositeFunc{
		Type: typ,
		Len:  len(obj.Elements),
	}, nil
}

// SetValue here is a no-op, because algorithmically when this is called from
// the func engine, the child elements (the list elements) will have had this
// done to them first, and as such when we try and retrieve the set value from
// this expression by calling `Value`, it will build it from scratch!
func (obj *ExprList) SetValue(value types.Value) error {
	if err := obj.typ.Cmp(value.Type()); err != nil {
		return err
	}
	// noop!
	//obj.V = value
	return nil
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
func (obj *ExprList) Value() (types.Value, error) {
	values := []types.Value{}
	var typ *types.Type

	for i, expr := range obj.Elements {
		etyp, err := expr.Type()
		if err != nil {
			return nil, errwrap.Wrapf(err, "list index `%d` did not return a type", i)
		}
		if typ == nil {
			typ = etyp
		}
		if err := typ.Cmp(etyp); err != nil {
			return nil, errwrap.Wrapf(err, "list elements have different types")
		}

		value, err := expr.Value()
		if err != nil {
			return nil, err
		}
		if value == nil {
			return nil, fmt.Errorf("value for list index `%d` was nil", i)
		}
		values = append(values, value)
	}
	if len(obj.Elements) > 0 {
		t := &types.Type{
			Kind: types.KindList,
			Val:  typ,
		}
		// Run SetType to ensure type is consistent with what we found,
		// which is an easy way to ensure the Cmp passes as expected...
		if err := obj.SetType(t); err != nil {
			return nil, errwrap.Wrapf(err, "type did not match expected!")
		}
	}

	return &types.ListValue{
		T: obj.typ,
		V: values,
	}, nil
}

// ExprMap is a representation of a (dictionary) map.
type ExprMap struct {
	typ *types.Type

	KVs []*ExprMapKV
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprMap) Apply(fn func(interfaces.Node) error) error {
	for _, x := range obj.KVs {
		if err := x.Key.Apply(fn); err != nil {
			return err
		}
		if err := x.Val.Apply(fn); err != nil {
			return err
		}
	}
	return fn(obj)
}

// String returns a short representation of this expression.
func (obj *ExprMap) String() string {
	var s []string
	for _, x := range obj.KVs {
		s = append(s, fmt.Sprintf("%s: %s", x.Key.String(), x.Val.String()))
	}
	return fmt.Sprintf("map(%s)", strings.Join(s, ", "))
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprMap) Init(data *interfaces.Data) error {
	for _, x := range obj.KVs {
		if err := x.Key.Init(data); err != nil {
			return err
		}
		if err := x.Val.Init(data); err != nil {
			return err
		}
	}
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *ExprMap) Interpolate() (interfaces.Expr, error) {
	kvs := []*ExprMapKV{}
	for _, x := range obj.KVs {
		interpolatedKey, err := x.Key.Interpolate()
		if err != nil {
			return nil, err
		}
		interpolatedVal, err := x.Val.Interpolate()
		if err != nil {
			return nil, err
		}
		kv := &ExprMapKV{
			Key: interpolatedKey,
			Val: interpolatedVal,
		}
		kvs = append(kvs, kv)
	}
	return &ExprMap{
		typ: obj.typ,
		KVs: kvs,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *ExprMap) SetScope(scope *interfaces.Scope) error {
	for _, x := range obj.KVs {
		if err := x.Key.SetScope(scope); err != nil {
			return err
		}
		if err := x.Val.SetScope(scope); err != nil {
			return err
		}
	}
	return nil
}

// SetType is used to set the type of this expression once it is known. This
// usually happens during type unification, but it can also happen during
// parsing if a type is specified explicitly. Since types are static and don't
// change on expressions, if you attempt to set a different type than what has
// previously been set (when not initially known) this will error.
func (obj *ExprMap) SetType(typ *types.Type) error {
	// TODO: should we ensure this is set to a KindMap ?
	if obj.typ != nil {
		return obj.typ.Cmp(typ) // if not set, ensure it doesn't change
	}
	obj.typ = typ // set
	return nil
}

// Type returns the type of this expression.
func (obj *ExprMap) Type() (*types.Type, error) {
	var ktyp, vtyp *types.Type
	var err error
	for i, x := range obj.KVs {
		// keys
		kt, e := x.Key.Type()
		if e != nil {
			err = errwrap.Wrapf(e, "map key, index `%d` did not return a type", i)
			break
		}
		if ktyp == nil {
			ktyp = kt
		}
		if e := ktyp.Cmp(kt); e != nil {
			err = errwrap.Wrapf(e, "key elements have different types")
			break
		}

		// vals
		vt, e := x.Val.Type()
		if e != nil {
			err = errwrap.Wrapf(e, "map val, index `%d` did not return a type", i)
			break
		}
		if vtyp == nil {
			vtyp = vt
		}
		if e := vtyp.Cmp(vt); e != nil {
			err = errwrap.Wrapf(e, "val elements have different types")
			break
		}
	}
	if err == nil && obj.typ == nil && len(obj.KVs) > 0 {
		return &types.Type{ // speculate!
			Kind: types.KindMap,
			Key:  ktyp,
			Val:  vtyp,
		}, nil
	}

	if obj.typ == nil {
		return nil, interfaces.ErrTypeCurrentlyUnknown
	}
	return obj.typ, nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprMap) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	// if this was set explicitly by the parser
	if obj.typ != nil {
		invar := &unification.EqualsInvariant{
			Expr: obj,
			Type: obj.typ,
		}
		invariants = append(invariants, invar)
	}

	// collect all the invariants of each sub-expression
	for _, x := range obj.KVs {
		keyInvars, err := x.Key.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, keyInvars...)

		valInvars, err := x.Val.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, valInvars...)
	}

	// all keys must have the same type, all vals must have the same type
	if len(obj.KVs) > 1 {
		keyExprs, valExprs := []interfaces.Expr{}, []interfaces.Expr{}
		for i := range obj.KVs {
			keyExprs = append(keyExprs, obj.KVs[i].Key)
			valExprs = append(valExprs, obj.KVs[i].Val)
		}

		keyInvariant := &unification.EqualityInvariantList{
			Exprs: keyExprs,
		}
		invariants = append(invariants, keyInvariant)

		valInvariant := &unification.EqualityInvariantList{
			Exprs: valExprs,
		}
		invariants = append(invariants, valInvariant)
	}

	// we should be type map of (type of element)
	if len(obj.KVs) > 0 {
		invariant := &unification.EqualityWrapMapInvariant{
			Expr1:    obj, // unique id for this expression (a pointer)
			Expr2Key: obj.KVs[0].Key,
			Expr2Val: obj.KVs[0].Val,
		}
		invariants = append(invariants, invariant)
	}

	// make sure this empty map gets a type for its key/value somehow
	if len(obj.KVs) == 0 {
		invariant := &unification.AnyInvariant{
			Expr: obj,
		}
		invariants = append(invariants, invariant)

		// build a placeholder expr to represent a contained key...
		exprAnyKey, exprAnyVal := &ExprAny{}, &ExprAny{}
		invarsKey, err := exprAnyKey.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invarsKey...)
		invarsVal, err := exprAnyVal.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invarsVal...)

		// FIXME: instead of using `ExprAny`, we could actually teach
		// our unification engine to ensure that our expr kind is list,
		// eg:
		//&unification.EqualityKindInvariant{
		//	Expr1: obj,
		//	Kind:  types.KindMap,
		//}
		invar := &unification.EqualityWrapMapInvariant{
			Expr1:    obj,
			Expr2Key: exprAnyKey, // hack
			Expr2Val: exprAnyVal, // hack
		}
		invariants = append(invariants, invar)
	}

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it, and
// the edges from all of the child graphs to this.
func (obj *ExprMap) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("map")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)

	// each map key value pair needs to point to the final map expression
	for index, x := range obj.KVs { // map fields in order
		g, err := x.Key.Graph()
		if err != nil {
			return nil, err
		}
		// do the key names ever change? -- yes
		fieldName := fmt.Sprintf("key:%d", index) // stringify map key
		edge := &funcs.Edge{Args: []string{fieldName}}

		var once bool
		edgeGenFn := func(v1, v2 pgraph.Vertex) pgraph.Edge {
			if once {
				panic(fmt.Sprintf("edgeGenFn for map, key `%s` was called twice", fieldName))
			}
			once = true
			return edge
		}
		graph.AddEdgeGraphVertexLight(g, obj, edgeGenFn) // key -> func
	}

	// each map key value pair needs to point to the final map expression
	for index, x := range obj.KVs { // map fields in order
		g, err := x.Val.Graph()
		if err != nil {
			return nil, err
		}
		fieldName := fmt.Sprintf("val:%d", index) // stringify map val
		edge := &funcs.Edge{Args: []string{fieldName}}

		var once bool
		edgeGenFn := func(v1, v2 pgraph.Vertex) pgraph.Edge {
			if once {
				panic(fmt.Sprintf("edgeGenFn for map, val `%s` was called twice", fieldName))
			}
			once = true
			return edge
		}
		graph.AddEdgeGraphVertexLight(g, obj, edgeGenFn) // val -> func
	}

	return graph, nil
}

// Func returns the reactive stream of values that this expression produces.
func (obj *ExprMap) Func() (interfaces.Func, error) {
	typ, err := obj.Type()
	if err != nil {
		return nil, err
	}

	// composite func (list, map, struct)
	return &structs.CompositeFunc{
		Type: typ, // the key/val types are known via this type
		Len:  len(obj.KVs),
	}, nil
}

// SetValue here is a no-op, because algorithmically when this is called from
// the func engine, the child key/value's (the map elements) will have had this
// done to them first, and as such when we try and retrieve the set value from
// this expression by calling `Value`, it will build it from scratch!
func (obj *ExprMap) SetValue(value types.Value) error {
	if err := obj.typ.Cmp(value.Type()); err != nil {
		return err
	}
	// noop!
	//obj.V = value
	return nil
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
func (obj *ExprMap) Value() (types.Value, error) {
	kvs := make(map[types.Value]types.Value)
	var ktyp, vtyp *types.Type

	for i, x := range obj.KVs {
		// keys
		kt, err := x.Key.Type()
		if err != nil {
			return nil, errwrap.Wrapf(err, "map key, index `%d` did not return a type", i)
		}
		if ktyp == nil {
			ktyp = kt
		}
		if err := ktyp.Cmp(kt); err != nil {
			return nil, errwrap.Wrapf(err, "key elements have different types")
		}

		key, err := x.Key.Value()
		if err != nil {
			return nil, err
		}
		if key == nil {
			return nil, fmt.Errorf("key for map index `%d` was nil", i)
		}

		// vals
		vt, err := x.Val.Type()
		if err != nil {
			return nil, errwrap.Wrapf(err, "map val, index `%d` did not return a type", i)
		}
		if vtyp == nil {
			vtyp = vt
		}
		if err := vtyp.Cmp(vt); err != nil {
			return nil, errwrap.Wrapf(err, "val elements have different types")
		}

		val, err := x.Val.Value()
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, fmt.Errorf("val for map index `%d` was nil", i)
		}

		kvs[key] = val // add to map
	}
	if len(obj.KVs) > 0 {
		t := &types.Type{
			Kind: types.KindMap,
			Key:  ktyp,
			Val:  vtyp,
		}
		// Run SetType to ensure type is consistent with what we found,
		// which is an easy way to ensure the Cmp passes as expected...
		if err := obj.SetType(t); err != nil {
			return nil, errwrap.Wrapf(err, "type did not match expected!")
		}
	}

	return &types.MapValue{
		T: obj.typ,
		V: kvs,
	}, nil
}

// ExprMapKV represents a key and value pair in a (dictionary) map. This does
// not satisfy the Expr interface.
type ExprMapKV struct {
	Key interfaces.Expr // keys can be strings, int's, etc...
	Val interfaces.Expr
}

// ExprStruct is a representation of a struct.
type ExprStruct struct {
	typ *types.Type

	Fields []*ExprStructField // the list (fields) are intentionally ordered!
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprStruct) Apply(fn func(interfaces.Node) error) error {
	for _, x := range obj.Fields {
		if err := x.Value.Apply(fn); err != nil {
			return err
		}
	}
	return fn(obj)
}

// String returns a short representation of this expression.
func (obj *ExprStruct) String() string {
	var s []string
	for _, x := range obj.Fields {
		s = append(s, fmt.Sprintf("%s: %s", x.Name, x.Value.String()))
	}
	return fmt.Sprintf("struct(%s)", strings.Join(s, "; "))
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprStruct) Init(data *interfaces.Data) error {
	for _, x := range obj.Fields {
		if err := x.Value.Init(data); err != nil {
			return err
		}
	}
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *ExprStruct) Interpolate() (interfaces.Expr, error) {
	fields := []*ExprStructField{}
	for _, x := range obj.Fields {
		interpolated, err := x.Value.Interpolate()
		if err != nil {
			return nil, err
		}
		field := &ExprStructField{
			Name:  x.Name, // don't interpolate the key
			Value: interpolated,
		}
		fields = append(fields, field)
	}
	return &ExprStruct{
		typ:    obj.typ,
		Fields: fields,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *ExprStruct) SetScope(scope *interfaces.Scope) error {
	for _, x := range obj.Fields {
		if err := x.Value.SetScope(scope); err != nil {
			return err
		}
	}
	return nil
}

// SetType is used to set the type of this expression once it is known. This
// usually happens during type unification, but it can also happen during
// parsing if a type is specified explicitly. Since types are static and don't
// change on expressions, if you attempt to set a different type than what has
// previously been set (when not initially known) this will error.
func (obj *ExprStruct) SetType(typ *types.Type) error {
	// TODO: should we ensure this is set to a KindStruct ?
	if obj.typ != nil {
		return obj.typ.Cmp(typ) // if not set, ensure it doesn't change
	}
	obj.typ = typ // set
	return nil
}

// Type returns the type of this expression.
func (obj *ExprStruct) Type() (*types.Type, error) {
	var m = make(map[string]*types.Type)
	ord := []string{}
	var err error
	for i, x := range obj.Fields {
		// vals
		t, e := x.Value.Type()
		if e != nil {
			err = errwrap.Wrapf(e, "field val, index `%d` did not return a type", i)
			break
		}
		if _, exists := m[x.Name]; exists {
			err = fmt.Errorf("struct type field index `%d` already exists", i)
			break
		}
		m[x.Name] = t
		ord = append(ord, x.Name)
	}
	if err == nil && obj.typ == nil && len(obj.Fields) > 0 {
		return &types.Type{ // speculate!
			Kind: types.KindStruct,
			Map:  m,
			Ord:  ord, // assume same order as fields
		}, nil
	}

	if obj.typ == nil {
		return nil, interfaces.ErrTypeCurrentlyUnknown
	}
	return obj.typ, nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprStruct) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	// if this was set explicitly by the parser
	if obj.typ != nil {
		invar := &unification.EqualsInvariant{
			Expr: obj,
			Type: obj.typ,
		}
		invariants = append(invariants, invar)
	}

	// collect all the invariants of each sub-expression
	for _, x := range obj.Fields {
		invars, err := x.Value.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invars...)
	}

	// build the reference to ourself if we have undetermined field types
	mapped := make(map[string]interfaces.Expr)
	ordered := []string{}
	for _, x := range obj.Fields {
		mapped[x.Name] = x.Value
		ordered = append(ordered, x.Name)
	}
	invariant := &unification.EqualityWrapStructInvariant{
		Expr1:    obj, // unique id for this expression (a pointer)
		Expr2Map: mapped,
		Expr2Ord: ordered,
	}
	invariants = append(invariants, invariant)

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it, and
// the edges from all of the child graphs to this.
func (obj *ExprStruct) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("struct")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)

	// each struct field needs to point to the final struct expression
	for _, x := range obj.Fields { // struct fields in order
		g, err := x.Value.Graph()
		if err != nil {
			return nil, err
		}

		fieldName := x.Name
		edge := &funcs.Edge{Args: []string{fieldName}}

		var once bool
		edgeGenFn := func(v1, v2 pgraph.Vertex) pgraph.Edge {
			if once {
				panic(fmt.Sprintf("edgeGenFn for struct, arg `%s` was called twice", fieldName))
			}
			once = true
			return edge
		}
		graph.AddEdgeGraphVertexLight(g, obj, edgeGenFn) // arg -> func
	}

	return graph, nil
}

// Func returns the reactive stream of values that this expression produces.
func (obj *ExprStruct) Func() (interfaces.Func, error) {
	typ, err := obj.Type()
	if err != nil {
		return nil, err
	}

	// composite func (list, map, struct)
	return &structs.CompositeFunc{
		Type: typ,
	}, nil
}

// SetValue here is a no-op, because algorithmically when this is called from
// the func engine, the child fields (the struct elements) will have had this
// done to them first, and as such when we try and retrieve the set value from
// this expression by calling `Value`, it will build it from scratch!
func (obj *ExprStruct) SetValue(value types.Value) error {
	if err := obj.typ.Cmp(value.Type()); err != nil {
		return err
	}
	// noop!
	//obj.V = value
	return nil
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
func (obj *ExprStruct) Value() (types.Value, error) {
	fields := make(map[string]types.Value)
	typ := &types.Type{
		Kind: types.KindStruct,
		Map:  make(map[string]*types.Type),
		//Ord:  obj.typ.Ord, // assume same order
	}
	ord := []string{} // can't use obj.typ b/c it can be nil during speculation

	for i, x := range obj.Fields {
		// vals
		t, err := x.Value.Type()
		if err != nil {
			return nil, errwrap.Wrapf(err, "field val, index `%d` did not return a type", i)
		}
		if _, exists := typ.Map[x.Name]; exists {
			return nil, fmt.Errorf("struct type field index `%d` already exists", i)
		}
		typ.Map[x.Name] = t

		val, err := x.Value.Value()
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, fmt.Errorf("val for field index `%d` was nil", i)
		}

		if _, exists := fields[x.Name]; exists {
			return nil, fmt.Errorf("struct field index `%d` already exists", i)
		}
		fields[x.Name] = val // add to map
		ord = append(ord, x.Name)
	}
	typ.Ord = ord
	if len(obj.Fields) > 0 {
		// Run SetType to ensure type is consistent with what we found,
		// which is an easy way to ensure the Cmp passes as expected...
		if err := obj.SetType(typ); err != nil {
			return nil, errwrap.Wrapf(err, "type did not match expected!")
		}
	}

	return &types.StructValue{
		T: obj.typ,
		V: fields,
	}, nil
}

// ExprStructField represents a name value pair in a struct field. This does not
// satisfy the Expr interface.
type ExprStructField struct {
	Name  string
	Value interfaces.Expr
}

// ExprFunc is a representation of a function value. This is not a function
// call, that is represented by ExprCall. This is what we build when we have a
// lambda that we want to express, or the contents of a StmtFunc that needs a
// function body (this ExprFunc) as well. This is used when the user defines an
// inline function in mcl code somewhere.
// XXX: this is currently not fully implemented, and parts may be incorrect.
type ExprFunc struct {
	Args   []*Arg
	Return *types.Type // return type if specified
	Body   interfaces.Expr

	typ *types.Type

	V func([]types.Value) (types.Value, error)
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprFunc) Apply(fn func(interfaces.Node) error) error {
	// TODO: is there anything to iterate in here?
	return fn(obj)
}

// String returns a short representation of this expression.
// FIXME: fmt.Sprintf("func(%+v)", obj.V) fails `go vet` (bug?), so wait until
// we have a better printable function value and put that here instead.
//func (obj *ExprFunc) String() string { return fmt.Sprintf("func(???)") } // TODO: print nicely
func (obj *ExprFunc) String() string {
	var a []string
	for _, x := range obj.Args {
		a = append(a, fmt.Sprintf("%s", x.String()))
	}
	args := strings.Join(a, ", ")
	s := fmt.Sprintf("func(%s)", args)
	if obj.Return != nil {
		s += fmt.Sprintf(" %s", obj.Return.String())
	}
	s += fmt.Sprintf(" { %s }", obj.Body.String())
	return s
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprFunc) Init(*interfaces.Data) error { return nil }

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// Here it simply returns itself, as no interpolation is possible.
func (obj *ExprFunc) Interpolate() (interfaces.Expr, error) {
	return &ExprFunc{
		V: obj.V,
	}, nil
}

// SetScope does nothing for this struct, because it has no child nodes, and it
// does not need to know about the parent scope.
// XXX: this may not be true in the future...
func (obj *ExprFunc) SetScope(*interfaces.Scope) error { return nil }

// SetType is used to set the type of this expression once it is known. This
// usually happens during type unification, but it can also happen during
// parsing if a type is specified explicitly. Since types are static and don't
// change on expressions, if you attempt to set a different type than what has
// previously been set (when not initially known) this will error.
func (obj *ExprFunc) SetType(typ *types.Type) error {
	// TODO: should we ensure this is set to a KindFunc ?
	if obj.typ != nil {
		return obj.typ.Cmp(typ) // if not set, ensure it doesn't change
	}
	obj.typ = typ // set
	return nil
}

// Type returns the type of this expression.
func (obj *ExprFunc) Type() (*types.Type, error) {
	// TODO: implement speculative type lookup (if not already sufficient)
	if obj.typ == nil {
		return nil, interfaces.ErrTypeCurrentlyUnknown
	}
	return obj.typ, nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprFunc) Unify() ([]interfaces.Invariant, error) {
	return nil, fmt.Errorf("not implemented") // XXX: not implemented
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it.
func (obj *ExprFunc) Graph() (*pgraph.Graph, error) {
	return nil, fmt.Errorf("not implemented") // XXX: not implemented
}

// Func returns the reactive stream of values that this expression produces.
func (obj *ExprFunc) Func() (interfaces.Func, error) {
	return nil, fmt.Errorf("not implemented") // XXX: not implemented
}

// SetValue for a func expression is always populated statically, and does not
// ever receive any incoming values (no incoming edges) so this should never be
// called. It has been implemented for uniformity.
func (obj *ExprFunc) SetValue(value types.Value) error {
	return obj.typ.Cmp(value.Type())
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
// This particular value is always known since it is a constant.
func (obj *ExprFunc) Value() (types.Value, error) {
	// TODO: implement speculative value lookup (if not already sufficient)
	return &types.FuncValue{
		V: obj.V,
		T: obj.typ,
	}, nil
}

// ExprCall is a representation of a function call. This does not represent the
// declaration or implementation of a new function value.
type ExprCall struct {
	scope *interfaces.Scope // store for referencing this later
	typ   *types.Type

	V types.Value // stored result (set with SetValue)

	Name string
	Args []interfaces.Expr // list of args in parsed order
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprCall) Apply(fn func(interfaces.Node) error) error {
	for _, x := range obj.Args {
		if err := x.Apply(fn); err != nil {
			return err
		}
	}
	return fn(obj)
}

// String returns a short representation of this expression.
func (obj *ExprCall) String() string {
	var s []string
	for _, x := range obj.Args {
		s = append(s, fmt.Sprintf("%s", x.String()))
	}
	return fmt.Sprintf("call:%s(%s)", obj.Name, strings.Join(s, ", "))
}

// buildType builds the KindFunc type of this function's signature if it can. It
// might not be able to if type unification hasn't yet been performed on this
// expression, and if SetType hasn't yet been called for the needed expressions.
// XXX: review this function logic please
func (obj *ExprCall) buildType() (*types.Type, error) {

	m := make(map[string]*types.Type)
	ord := []string{}
	for pos, x := range obj.Args { // function arguments in order
		t, err := x.Type()
		if err != nil {
			return nil, err
		}
		name := util.NumToAlpha(pos) // assume (incorrectly) for now...
		//name := argNames[pos]
		m[name] = t
		ord = append(ord, name)
	}

	out, err := obj.Type()
	if err != nil {
		return nil, err
	}

	return &types.Type{
		Kind: types.KindFunc,
		Map:  m,
		Ord:  ord,
		Out:  out,
	}, nil
}

// buildFunc prepares and returns the function struct object needed for running
// this function execution.
// XXX: review this function logic please
func (obj *ExprCall) buildFunc() (interfaces.Func, error) {
	// lookup function from scope
	f, exists := obj.scope.Functions[obj.Name]
	if !exists {
		return nil, fmt.Errorf("func `%s` does not exist in this scope", obj.Name)
	}
	fn := f() // build

	polyFn, ok := fn.(interfaces.PolyFunc) // is it statically polymorphic?
	if !ok {
		return fn, nil
	}

	// PolyFunc's need more things done!
	typ, err := obj.buildType()
	if err == nil { // if we've errored, that's okay, this part isn't ready
		if err := polyFn.Build(typ); err != nil {
			return nil, errwrap.Wrapf(err, "could not build func `%s`", obj.Name)
		}
	}
	return fn, nil
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprCall) Init(data *interfaces.Data) error {
	for _, x := range obj.Args {
		if err := x.Init(data); err != nil {
			return err
		}
	}
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *ExprCall) Interpolate() (interfaces.Expr, error) {
	args := []interfaces.Expr{}
	for _, x := range obj.Args {
		interpolated, err := x.Interpolate()
		if err != nil {
			return nil, err
		}
		args = append(args, interpolated)
	}
	return &ExprCall{
		typ:  obj.typ,
		Name: obj.Name,
		Args: args,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *ExprCall) SetScope(scope *interfaces.Scope) error {
	if scope == nil {
		scope = interfaces.EmptyScope()
	}
	obj.scope = scope

	for _, x := range obj.Args {
		if err := x.SetScope(scope); err != nil {
			return err
		}
	}
	return nil
}

// SetType is used to set the type of this expression once it is known. This
// usually happens during type unification, but it can also happen during
// parsing if a type is specified explicitly. Since types are static and don't
// change on expressions, if you attempt to set a different type than what has
// previously been set (when not initially known) this will error. Remember that
// for this function expression, the type is the *return type* of the function,
// not the full type of the function signature.
func (obj *ExprCall) SetType(typ *types.Type) error {
	if obj.typ != nil {
		return obj.typ.Cmp(typ) // if not set, ensure it doesn't change
	}
	obj.typ = typ // set
	return nil
}

// Type returns the type of this expression, which is the return type of the
// function call.
func (obj *ExprCall) Type() (*types.Type, error) {
	f, exists := obj.scope.Functions[obj.Name]
	if !exists {
		return nil, fmt.Errorf("func `%s` does not exist in this scope", obj.Name)
	}
	fn := f() // build

	_, isPoly := fn.(interfaces.PolyFunc) // is it statically polymorphic?
	if obj.typ == nil && !isPoly {
		if info := fn.Info(); info != nil {
			if sig := info.Sig; sig != nil {
				if typ := sig.Out; typ != nil && !typ.HasVariant() {
					return typ, nil // speculate!
				}
			}
		}
	}

	if obj.typ == nil {
		return nil, interfaces.ErrTypeCurrentlyUnknown
	}
	return obj.typ, nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprCall) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	// if this was set explicitly by the parser
	if obj.typ != nil {
		invar := &unification.EqualsInvariant{
			Expr: obj,
			Type: obj.typ,
		}
		invariants = append(invariants, invar)
	}

	// collect all the invariants of each sub-expression
	for _, x := range obj.Args {
		invars, err := x.Unify()
		if err != nil {
			return nil, err
		}
		invariants = append(invariants, invars...)
	}

	fn, err := obj.buildFunc() // uses obj.Name to build the func
	if err != nil {
		return nil, err
	}

	// XXX: can we put this inside the poly branch or is it needed everywhere?
	// XXX: is there code we can pull out of this branch to use for all functions?
	argNames := []string{}
	mapped := make(map[string]*types.Type)
	partialValues := []types.Value{}
	for i := range obj.Args {
		name := util.NumToAlpha(i) // assume (incorrectly) for now...
		argNames = append(argNames, name)
		mapped[name] = nil                         // unknown type
		partialValues = append(partialValues, nil) // XXX: is this safe?

		// optimization: if zeroth arg is a static string, specify this!
		// TODO: this is a more specialized version of the next check...
		if x, ok := obj.Args[0].(*ExprStr); i == 0 && ok { // is static?
			mapped[name], _ = x.Type()
			partialValues[i], _ = x.Value() // store value
		}

		// optimization: if type is already known, specify it now!
		if t, err := obj.Args[i].Type(); err == nil { // is known?
			mapped[name] = t
			// if value is completely static, pass it in now!
			if v, err := obj.Args[i].Value(); err == nil {
				partialValues[i] = v // store value
			}
		}
	}

	// do we have a special case like the operator or template function?
	polyFn, ok := fn.(interfaces.PolyFunc) // is it statically polymorphic?
	if ok {
		out, err := obj.Type() // do we know the return type yet?
		if err != nil {
			out = nil // just to make sure...
		}
		// partial type can have some type components that are nil!
		// this means they are not yet known at this time...
		partialType := &types.Type{
			Kind: types.KindFunc,
			Map:  mapped,
			Ord:  argNames,
			Out:  out, // possibly nil
		}

		results, err := polyFn.Polymorphisms(partialType, partialValues)
		if err != nil {
			return nil, errwrap.Wrapf(err, "polymorphic signatures for func `%s` could not be found", obj.Name)
		}

		ors := []interfaces.Invariant{} // solve only one from this list
		// each of these is a different possible signature
		for _, typ := range results {
			if typ.Kind != types.KindFunc {
				panic("overloaded result was not of kind func")
			}

			// XXX: how do we deal with template returning a variant?
			// XXX: i think we need more invariant types, and if it's
			// going to be a variant, just return no results, and the
			// defaults from the engine should just match it anyways!
			if typ.HasVariant() { // XXX: ¯\_(ツ)_/¯
				//continue // XXX: alternate strategy...
				//return nil, fmt.Errorf("variant type not yet supported, got: %+v", typ) // XXX: old strategy
			}
			if typ.Kind == types.KindVariant { // XXX: ¯\_(ツ)_/¯
				continue // can't deal with raw variant a.t.m.
			}

			if i, j := len(typ.Ord), len(obj.Args); i != j {
				continue // this signature won't work for us, skip!
			}

			// what would a set of invariants for this sig look like?
			var invars []interfaces.Invariant

			// use Map and Ord for Input (Kind == Function)
			for i, x := range typ.Ord {
				if typ.Map[x].HasVariant() { // XXX: ¯\_(ツ)_/¯
					invar := &unification.AnyInvariant{ // XXX: ???
						Expr: obj.Args[i],
					}
					invars = append(invars, invar)
					continue
				}
				invar := &unification.EqualsInvariant{
					Expr: obj.Args[i],
					Type: typ.Map[x], // type of arg
				}
				invars = append(invars, invar)
			}
			if typ.Out != nil {
				// this expression should equal the output type of the function
				if typ.Out.HasVariant() { // XXX: ¯\_(ツ)_/¯
					invar := &unification.AnyInvariant{ // XXX: ???
						Expr: obj,
					}
					invars = append(invars, invar)
				} else {
					invar := &unification.EqualsInvariant{
						Expr: obj,
						Type: typ.Out,
					}
					invars = append(invars, invar)
				}
			}

			// add more invariants to link the partials...
			mapped := make(map[string]interfaces.Expr)
			ordered := []string{}
			for pos, x := range obj.Args {
				name := argNames[pos]
				mapped[name] = x
				ordered = append(ordered, name)
			}

			// unused expression, here only for linking...
			// TODO: eventually like with proper ExprFunc in lang?
			exprFunc := &ExprFunc{}
			if !typ.HasVariant() { // XXX: ¯\_(ツ)_/¯
				exprFunc.SetType(typ)
				funcInvariant := &unification.EqualsInvariant{
					Expr: exprFunc,
					Type: typ,
				}
				invars = append(invars, funcInvariant)
			}
			invar := &unification.EqualityWrapFuncInvariant{
				Expr1:    exprFunc,
				Expr2Map: mapped,
				Expr2Ord: ordered,
				Expr2Out: obj, // type of expression is return type of function
			}
			invars = append(invars, invar)

			// all of these need to be true together
			and := &unification.ConjunctionInvariant{
				Invariants: invars,
			}

			ors = append(ors, and) // one solution added!
		} // end results loop

		// don't error here, we might not want to add any invariants!
		//if len(results) == 0 {
		//	return nil, fmt.Errorf("can't find any valid signatures that match func `%s`", obj.Name)
		//}
		if len(ors) > 0 {
			var invar interfaces.Invariant = &unification.ExclusiveInvariant{
				Invariants: ors, // one and only one of these should be true
			}
			if len(ors) == 1 {
				invar = ors[0] // there should only be one
			}
			invariants = append(invariants, invar)
		}

	} else {
		sig := fn.Info().Sig
		// build the reference to ourself if we have undetermined arg types
		mapped := make(map[string]interfaces.Expr)
		ordered := []string{}
		for pos, x := range obj.Args {
			name := argNames[pos]
			mapped[name] = x
			ordered = append(ordered, name)
		}

		// add an unused expression, because we need to link it to the partial
		exprFunc := &ExprFunc{}
		exprFunc.SetType(sig)
		funcInvariant := &unification.EqualsInvariant{
			Expr: exprFunc,
			Type: sig,
		}
		invariants = append(invariants, funcInvariant)

		// note: the usage of this invariant is different from the other wrap*
		// invariants, because in this case, the expression type is the return
		// type which is produced, where as the entire function itself has its
		// own type which includes the types of the input arguments...
		invariant := &unification.EqualityWrapFuncInvariant{
			Expr1:    exprFunc, // unused placeholder for unification
			Expr2Map: mapped,
			Expr2Ord: ordered,
			Expr2Out: obj, // type of expression is return type of function
		}
		invariants = append(invariants, invariant)
	}

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it, and
// the edges from all of the child graphs to this.
func (obj *ExprCall) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("func")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)

	fn, err := obj.buildFunc() // uses obj.Name to build the func
	if err != nil {
		return nil, err
	}
	argNames := fn.Info().Sig.Ord
	if len(argNames) != len(obj.Args) { // extra safety...
		return nil, fmt.Errorf("func `%s` expected %d args, got %d", obj.Name, len(argNames), len(obj.Args))
	}

	// each function argument needs to point to the final function expression
	for pos, x := range obj.Args { // function arguments in order
		g, err := x.Graph()
		if err != nil {
			return nil, err
		}

		argName := argNames[pos]
		edge := &funcs.Edge{Args: []string{argName}}

		var once bool
		edgeGenFn := func(v1, v2 pgraph.Vertex) pgraph.Edge {
			if once {
				panic(fmt.Sprintf("edgeGenFn for func `%s`, arg `%s` was called twice", obj.Name, argName))
			}
			once = true
			return edge
		}
		graph.AddEdgeGraphVertexLight(g, obj, edgeGenFn) // arg -> func
	}

	return graph, nil
}

// Func returns the reactive stream of values that this expression produces.
func (obj *ExprCall) Func() (interfaces.Func, error) {
	return obj.buildFunc() // uses obj.Name to build the func
}

// SetValue here is used to store the result of the last computation of this
// expression node after it has received all the required input values. This
// value is cached and can be retrieved by calling Value.
func (obj *ExprCall) SetValue(value types.Value) error {
	if err := obj.typ.Cmp(value.Type()); err != nil {
		return err
	}
	obj.V = value
	return nil
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
// It is often unlikely that this kind of speculative execution finds something.
// This particular implementation of the function returns the previously stored
// and cached value as received by SetValue.
func (obj *ExprCall) Value() (types.Value, error) {
	if obj.V == nil {
		return nil, fmt.Errorf("func value does not yet exist")
	}
	return obj.V, nil
}

// ExprVar is a representation of a variable lookup. It returns the expression
// that that variable refers to.
type ExprVar struct {
	scope *interfaces.Scope // store for referencing this later
	typ   *types.Type

	Name string // name of the variable
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprVar) Apply(fn func(interfaces.Node) error) error { return fn(obj) }

// String returns a short representation of this expression.
func (obj *ExprVar) String() string { return fmt.Sprintf("var(%s)", obj.Name) }

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprVar) Init(*interfaces.Data) error { return nil }

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
// Here it returns itself, since variable names cannot be interpolated. We don't
// support variable, variables or anything crazy like that.
func (obj *ExprVar) Interpolate() (interfaces.Expr, error) {
	return &ExprVar{
		Name: obj.Name,
	}, nil
}

// SetScope stores the scope for use in this resource.
func (obj *ExprVar) SetScope(scope *interfaces.Scope) error {
	if scope == nil {
		scope = interfaces.EmptyScope()
	}
	obj.scope = scope
	return nil
}

// SetType is used to set the type of this expression once it is known. This
// usually happens during type unification, but it can also happen during
// parsing if a type is specified explicitly. Since types are static and don't
// change on expressions, if you attempt to set a different type than what has
// previously been set (when not initially known) this will error.
func (obj *ExprVar) SetType(typ *types.Type) error {
	if obj.typ != nil {
		return obj.typ.Cmp(typ) // if not set, ensure it doesn't change
	}
	obj.typ = typ // set
	return nil
}

// Type returns the type of this expression.
func (obj *ExprVar) Type() (*types.Type, error) {
	// return type if it is already known statically...
	// it is useful for type unification to have some extra info
	expr, exists := obj.scope.Variables[obj.Name]
	// if !exists, just ignore the error for now since this is speculation!
	// this logic simplifies down to just this!
	if exists && obj.typ == nil {
		return expr.Type()
	}

	if obj.typ == nil {
		return nil, interfaces.ErrTypeCurrentlyUnknown
	}
	return obj.typ, nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprVar) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	// lookup value from scope
	expr, exists := obj.scope.Variables[obj.Name]
	if !exists {
		return nil, fmt.Errorf("var `%s` does not exist in this scope", obj.Name)
	}

	// if this was set explicitly by the parser
	if obj.typ != nil {
		invar := &unification.EqualsInvariant{
			Expr: obj,
			Type: obj.typ,
		}
		invariants = append(invariants, invar)
	}

	// don't recurse because we already got this through the bind statement
	//invars, err := expr.Unify()
	//if err != nil {
	//	return nil, err
	//}
	//invariants = append(invariants, invars...)

	// this expression's type must be the type of what the var is bound to!
	// TODO: does this always cause an identical duplicate invariant?
	invar := &unification.EqualityInvariant{
		Expr1: obj,
		Expr2: expr,
	}
	invariants = append(invariants, invar)

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This returns a graph with a single vertex (itself) in it, and
// the edges from all of the child graphs to this. The child graph in this case
// is the graph which is obtained from the bound expression. The edge points
// from that expression to this vertex. The function used for this vertex is a
// simple placeholder which sucks incoming values in and passes them on. This is
// important for filling the logical requirements of the graph type checker, and
// to avoid duplicating production of the incoming input value from the bound
// expression.
func (obj *ExprVar) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("var")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)

	// ??? = $foo (this is the foo)
	// lookup value from scope
	expr, exists := obj.scope.Variables[obj.Name]
	if !exists {
		return nil, fmt.Errorf("var `%s` does not exist in this scope", obj.Name)
	}

	// should already exist in graph (i think)...
	graph.AddVertex(expr) // duplicate additions are ignored and are harmless

	// the expr needs to point to the var lookup expression
	g, err := expr.Graph()
	if err != nil {
		return nil, err
	}

	edge := &funcs.Edge{Args: []string{obj.Name}}

	var once bool
	edgeGenFn := func(v1, v2 pgraph.Vertex) pgraph.Edge {
		if once {
			panic(fmt.Sprintf("edgeGenFn for var `%s` was called twice", obj.Name))
		}
		once = true
		return edge
	}
	graph.AddEdgeGraphVertexLight(g, obj, edgeGenFn) // expr -> var

	return graph, nil
}

// Func returns a "pass-through" function which receives the bound value, and
// passes it to the consumer. This is essential for satisfying the type checker
// of the function graph engine.
func (obj *ExprVar) Func() (interfaces.Func, error) {
	expr, exists := obj.scope.Variables[obj.Name]
	if !exists {
		return nil, fmt.Errorf("var `%s` does not exist in scope", obj.Name)
	}

	// this is wrong, if we did it this way, this expr wouldn't exist as a
	// distinct node in the function graph to relay values through, instead,
	// it would be acting as a "substitution/lookup" function, which just
	// copies the bound function over into here. As a result, we'd have N
	// copies of that function (based on the number of times N that that
	// variable is used) instead of having that single bound function as
	// input which is sent via N different edges to the multiple locations
	// where the variables are used. Since the bound function would still
	// have a single unique pointer, this wouldn't actually exist more than
	// once in the graph, although since it's illogical, it causes the graph
	// type checking (the edge counting in the function graph engine) to
	// notice a problem and error.
	//return expr.Func() // recurse?

	// instead, return a function which correctly does a lookup in the scope
	// and returns *that* stream of values instead.
	typ, err := obj.Type()
	if err != nil {
		return nil, err
	}

	f, err := expr.Func()
	if err != nil {
		return nil, err
	}

	// var func
	return &structs.VarFunc{
		Type: typ,
		Func: f,
		Edge: obj.Name, // the edge name used above in Graph is this...
	}, nil
}

// SetValue here is a no-op, because algorithmically when this is called from
// the func engine, the child fields (the dest lookup expr) will have had this
// done to them first, and as such when we try and retrieve the set value from
// this expression by calling `Value`, it will build it from scratch!
func (obj *ExprVar) SetValue(value types.Value) error {
	if err := obj.typ.Cmp(value.Type()); err != nil {
		return err
	}
	// noop!
	//obj.V = value
	return nil
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
// This returns the value this variable points to. It is able to do so because
// it can lookup in the previous set scope which expression this points to, and
// then it can call Value on that expression.
func (obj *ExprVar) Value() (types.Value, error) {
	expr, exists := obj.scope.Variables[obj.Name]
	if !exists {
		return nil, fmt.Errorf("var `%s` does not exist in scope", obj.Name)
	}
	return expr.Value() // recurse
}

// Arg represents a name identifier for a func or class argument declaration and
// is sometimes accompanied by a type. This does not satisfy the Expr interface.
type Arg struct {
	Name string
	Type *types.Type // nil if unspecified (needs to be solved for)
}

// String returns a short representation of this arg.
func (obj *Arg) String() string {
	s := obj.Name
	if obj.Type != nil {
		s += fmt.Sprintf(" %s", obj.Type.String())
	}
	return s
}

// ExprIf represents an if expression which *must* have both branches, and which
// returns a value. As a result, it has a type. This is different from a StmtIf,
// which does not need to have both branches, and which does not return a value.
type ExprIf struct {
	typ *types.Type

	Condition  interfaces.Expr
	ThenBranch interfaces.Expr // could be an ExprBranch
	ElseBranch interfaces.Expr // could be an ExprBranch
}

// Apply is a general purpose iterator method that operates on any AST node. It
// is not used as the primary AST traversal function because it is less readable
// and easy to reason about than manually implementing traversal for each node.
// Nevertheless, it is a useful facility for operations that might only apply to
// a select number of node types, since they won't need extra noop iterators...
func (obj *ExprIf) Apply(fn func(interfaces.Node) error) error {
	if err := obj.Condition.Apply(fn); err != nil {
		return err
	}
	if err := obj.ThenBranch.Apply(fn); err != nil {
		return err
	}
	if err := obj.ElseBranch.Apply(fn); err != nil {
		return err
	}
	return fn(obj)
}

// String returns a short representation of this expression.
func (obj *ExprIf) String() string {
	return fmt.Sprintf("if(%s)", obj.Condition.String()) // TODO: improve this
}

// Init initializes this branch of the AST, and returns an error if it fails to
// validate.
func (obj *ExprIf) Init(data *interfaces.Data) error {
	if err := obj.Condition.Init(data); err != nil {
		return err
	}
	if err := obj.ThenBranch.Init(data); err != nil {
		return err
	}
	if err := obj.ElseBranch.Init(data); err != nil {
		return err
	}
	return nil
}

// Interpolate returns a new node (aka a copy) once it has been expanded. This
// generally increases the size of the AST when it is used. It calls Interpolate
// on any child elements and builds the new node with those new node contents.
func (obj *ExprIf) Interpolate() (interfaces.Expr, error) {
	condition, err := obj.Condition.Interpolate()
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not interpolate Condition")
	}
	thenBranch, err := obj.ThenBranch.Interpolate()
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not interpolate ThenBranch")
	}
	elseBranch, err := obj.ElseBranch.Interpolate()
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not interpolate ElseBranch")
	}
	return &ExprIf{
		typ:        obj.typ,
		Condition:  condition,
		ThenBranch: thenBranch,
		ElseBranch: elseBranch,
	}, nil
}

// SetScope stores the scope for later use in this resource and it's children,
// which it propagates this downwards to.
func (obj *ExprIf) SetScope(scope *interfaces.Scope) error {
	if err := obj.ThenBranch.SetScope(scope); err != nil {
		return err
	}
	if err := obj.ElseBranch.SetScope(scope); err != nil {
		return err
	}
	return obj.Condition.SetScope(scope)
}

// SetType is used to set the type of this expression once it is known. This
// usually happens during type unification, but it can also happen during
// parsing if a type is specified explicitly. Since types are static and don't
// change on expressions, if you attempt to set a different type than what has
// previously been set (when not initially known) this will error.
func (obj *ExprIf) SetType(typ *types.Type) error {
	if obj.typ != nil {
		return obj.typ.Cmp(typ) // if not set, ensure it doesn't change
	}
	obj.typ = typ // set
	return nil
}

// Type returns the type of this expression.
func (obj *ExprIf) Type() (*types.Type, error) {
	boolValue, err := obj.Condition.Value() // attempt early speculation
	if err == nil && obj.typ == nil {
		branch := obj.ElseBranch
		if boolValue.Bool() { // must not panic
			branch = obj.ThenBranch
		}
		return branch.Type()
	}

	if obj.typ == nil {
		return nil, interfaces.ErrTypeCurrentlyUnknown
	}
	return obj.typ, nil
}

// Unify returns the list of invariants that this node produces. It recursively
// calls Unify on any children elements that exist in the AST, and returns the
// collection to the caller.
func (obj *ExprIf) Unify() ([]interfaces.Invariant, error) {
	var invariants []interfaces.Invariant

	// if this was set explicitly by the parser
	if obj.typ != nil {
		invar := &unification.EqualsInvariant{
			Expr: obj,
			Type: obj.typ,
		}
		invariants = append(invariants, invar)
	}

	// conditional expression might have some children invariants to share
	condition, err := obj.Condition.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, condition...)

	// the condition must ultimately be a boolean
	conditionInvar := &unification.EqualsInvariant{
		Expr: obj.Condition,
		Type: types.TypeBool,
	}
	invariants = append(invariants, conditionInvar)

	// recurse into the two branches
	thenBranch, err := obj.ThenBranch.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, thenBranch...)

	elseBranch, err := obj.ElseBranch.Unify()
	if err != nil {
		return nil, err
	}
	invariants = append(invariants, elseBranch...)

	// the two branches must be equally typed
	branchesInvar := &unification.EqualityInvariant{
		Expr1: obj.ThenBranch,
		Expr2: obj.ElseBranch,
	}
	invariants = append(invariants, branchesInvar)

	// the two branches must match the type of the whole expression
	thenInvar := &unification.EqualityInvariant{
		Expr1: obj,
		Expr2: obj.ThenBranch,
	}
	invariants = append(invariants, thenInvar)
	elseInvar := &unification.EqualityInvariant{
		Expr1: obj,
		Expr2: obj.ElseBranch,
	}
	invariants = append(invariants, elseInvar)

	return invariants, nil
}

// Graph returns the reactive function graph which is expressed by this node. It
// includes any vertices produced by this node, and the appropriate edges to any
// vertices that are produced by its children. Nodes which fulfill the Expr
// interface directly produce vertices (and possible children) where as nodes
// that fulfill the Stmt interface do not produces vertices, where as their
// children might. This particular if expression doesn't do anything clever here
// other than adding in both branches of the graph. Since we're functional, this
// shouldn't have any ill effects.
// XXX: is this completely true if we're running technically impure, but safe
// built-in functions on both branches? Can we turn off half of this?
func (obj *ExprIf) Graph() (*pgraph.Graph, error) {
	graph, err := pgraph.NewGraph("if")
	if err != nil {
		return nil, errwrap.Wrapf(err, "could not create graph")
	}
	graph.AddVertex(obj)

	exprs := map[string]interfaces.Expr{
		"c": obj.Condition,
		"a": obj.ThenBranch,
		"b": obj.ElseBranch,
	}
	for _, argName := range []string{"c", "a", "b"} { // deterministic order
		x := exprs[argName]
		g, err := x.Graph()
		if err != nil {
			return nil, err
		}

		edge := &funcs.Edge{Args: []string{argName}}

		var once bool
		edgeGenFn := func(v1, v2 pgraph.Vertex) pgraph.Edge {
			if once {
				panic(fmt.Sprintf("edgeGenFn for ifexpr edge `%s` was called twice", argName))
			}
			once = true
			return edge
		}
		graph.AddEdgeGraphVertexLight(g, obj, edgeGenFn) // branch -> if
	}

	return graph, nil
}

// Func returns a function which returns the correct branch based on the ever
// changing conditional boolean input.
func (obj *ExprIf) Func() (interfaces.Func, error) {
	typ, err := obj.Type()
	if err != nil {
		return nil, err
	}

	return &structs.IfFunc{
		Type: typ, // this is the output type of the expression
	}, nil
}

// SetValue here is a no-op, because algorithmically when this is called from
// the func engine, the child fields (the branches expr's) will have had this
// done to them first, and as such when we try and retrieve the set value from
// this expression by calling `Value`, it will build it from scratch!
func (obj *ExprIf) SetValue(value types.Value) error {
	if err := obj.typ.Cmp(value.Type()); err != nil {
		return err
	}
	// noop!
	//obj.V = value
	return nil
}

// Value returns the value of this expression in our type system. This will
// usually only be valid once the engine has run and values have been produced.
// This might get called speculatively (early) during unification to learn more.
// This particular expression evaluates the condition and returns the correct
// branch's value accordingly.
func (obj *ExprIf) Value() (types.Value, error) {
	boolValue, err := obj.Condition.Value()
	if err != nil {
		return nil, err
	}
	if boolValue.Bool() { // must not panic
		return obj.ThenBranch.Value()
	}
	return obj.ElseBranch.Value()
}
