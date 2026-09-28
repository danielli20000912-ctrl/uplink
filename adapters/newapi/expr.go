package newapi

// Translation of New API billing expressions (billing_mode = tiered_expr) into
// uplink prices. Only the canonical shapes produced by New API's tier editor are
// translated: a chain of len conditions whose leaves are tier(name, sum) with
// sum a plain linear combination of p, c, cr, cc, cc1h, or a single
// unconditional tier(name, fixed(amount)). Anything else is reported as
// unsupported and published as custom pricing.
//
// This is an independent parser for that subset; it does not use New API code.

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"unicode"
)

var errUnsupported = errors.New("unsupported billing expression")

// exprLeaf is one priced branch. Coefficients are USD per 1M tokens (token
// leaves) or USD per request (fixed leaves). A nil coefficient means the
// variable does not appear in the leaf.
type exprLeaf struct {
	fixed *big.Rat
	coef  map[string]*big.Rat // keys: p, c, cr, cc, cc1h
}

func (l *exprLeaf) equal(o *exprLeaf) bool {
	if (l.fixed == nil) != (o.fixed == nil) {
		return false
	}
	if l.fixed != nil {
		return l.fixed.Cmp(o.fixed) == 0
	}
	if len(l.coef) != len(o.coef) {
		return false
	}
	for k, v := range l.coef {
		w, ok := o.coef[k]
		if !ok || v.Cmp(w) != 0 {
			return false
		}
	}
	return true
}

// exprLevel is a leaf that applies when context tokens are greater than above.
// The first level has above = -1 (applies from zero).
type exprLevel struct {
	above int64
	leaf  *exprLeaf
}

// parseBillingExpr returns levels ordered by above ascending.
func parseBillingExpr(src string) ([]exprLevel, error) {
	src = strings.TrimSpace(src)
	if strings.Contains(src, "|||") {
		return nil, fmt.Errorf("%w: request rules", errUnsupported)
	}
	if strings.HasPrefix(src, "v1:") {
		src = src[3:]
	} else if len(src) > 2 && src[0] == 'v' && strings.Contains(src[:4], ":") {
		return nil, fmt.Errorf("%w: unknown expression version", errUnsupported)
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	root, err := p.parseCond()
	if err != nil {
		return nil, err
	}
	if !p.done() {
		return nil, fmt.Errorf("%w: trailing input %q", errUnsupported, p.peek().text)
	}

	// Breakpoints b such that some condition is exactly "len <= b".
	set := map[int64]bool{}
	root.breakpoints(set)
	bps := make([]int64, 0, len(set))
	for b := range set {
		bps = append(bps, b)
	}
	sort.Slice(bps, func(i, j int) bool { return bps[i] < bps[j] })

	// Evaluate each interval at a representative length.
	var levels []exprLevel
	add := func(above, sample int64) {
		leaf := root.eval(sample)
		if n := len(levels); n > 0 && levels[n-1].leaf.equal(leaf) {
			return
		}
		levels = append(levels, exprLevel{above: above, leaf: leaf})
	}
	if len(bps) == 0 {
		add(-1, 0)
	} else {
		add(-1, bps[0])
		for i := 0; i < len(bps); i++ {
			add(bps[i], bps[i]+1)
		}
	}
	return levels, nil
}

// ---- AST ----

type node interface {
	eval(length int64) *exprLeaf
	breakpoints(map[int64]bool)
}

// condNode: if len <= bound then yes else no.
type condNode struct {
	bound   int64
	yes, no node
}

func (n *condNode) eval(length int64) *exprLeaf {
	if length <= n.bound {
		return n.yes.eval(length)
	}
	return n.no.eval(length)
}

func (n *condNode) breakpoints(s map[int64]bool) {
	s[n.bound] = true
	n.yes.breakpoints(s)
	n.no.breakpoints(s)
}

type leafNode struct{ leaf *exprLeaf }

func (n *leafNode) eval(int64) *exprLeaf       { return n.leaf }
func (n *leafNode) breakpoints(map[int64]bool) {}

// ---- lexer ----

type tokKind int

const (
	tIdent tokKind = iota
	tNum
	tStr
	tOp
)

type token struct {
	kind tokKind
	text string
}

func lex(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(s) && (unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j])) || s[j] == '_') {
				j++
			}
			out = append(out, token{tIdent, s[i:j]})
			i = j
		case unicode.IsDigit(c) || (c == '.' && i+1 < len(s) && unicode.IsDigit(rune(s[i+1]))):
			j := i
			for j < len(s) && (unicode.IsDigit(rune(s[j])) || s[j] == '.' || s[j] == '_') {
				j++
			}
			if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
				return nil, fmt.Errorf("%w: exponent literal", errUnsupported)
			}
			out = append(out, token{tNum, strings.ReplaceAll(s[i:j], "_", "")})
			i = j
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(s) && s[j] != s[i] {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("%w: unterminated string", errUnsupported)
			}
			out = append(out, token{tStr, s[i+1 : j]})
			i = j + 1
		default:
			two := ""
			if i+1 < len(s) {
				two = s[i : i+2]
			}
			switch two {
			case "<=", ">=", "==", "!=", "&&", "||":
				out = append(out, token{tOp, two})
				i += 2
				continue
			}
			if strings.ContainsRune("<>?:(),*+-/", c) {
				out = append(out, token{tOp, string(c)})
				i++
				continue
			}
			return nil, fmt.Errorf("%w: unexpected character %q", errUnsupported, c)
		}
	}
	return out, nil
}

// ---- parser ----

type parser struct {
	toks []token
	pos  int
}

func (p *parser) done() bool { return p.pos >= len(p.toks) }

func (p *parser) peek() token {
	if p.done() {
		return token{tOp, "<eof>"}
	}
	return p.toks[p.pos]
}

func (p *parser) next() token {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) expect(kind tokKind, text string) error {
	t := p.next()
	if t.kind != kind || (text != "" && t.text != text) {
		return fmt.Errorf("%w: expected %q, got %q", errUnsupported, text, t.text)
	}
	return nil
}

// cond := "(" cond ")" | comparison "?" cond ":" cond | leaf
func (p *parser) parseCond() (node, error) {
	t := p.peek()
	if t.kind == tOp && t.text == "(" {
		p.next()
		n, err := p.parseCond()
		if err != nil {
			return nil, err
		}
		if err := p.expect(tOp, ")"); err != nil {
			return nil, err
		}
		return n, nil
	}
	if t.kind == tIdent && t.text == "len" {
		bound, swap, err := p.parseComparison()
		if err != nil {
			return nil, err
		}
		if err := p.expect(tOp, "?"); err != nil {
			return nil, err
		}
		yes, err := p.parseCond()
		if err != nil {
			return nil, err
		}
		if err := p.expect(tOp, ":"); err != nil {
			return nil, err
		}
		no, err := p.parseCond()
		if err != nil {
			return nil, err
		}
		if swap {
			yes, no = no, yes
		}
		return &condNode{bound: bound, yes: yes, no: no}, nil
	}
	return p.parseLeaf()
}

// comparison := "len" op INT. Returns b and swap such that the condition is
// "len <= b" (swap=false) or its negation (swap=true).
func (p *parser) parseComparison() (int64, bool, error) {
	p.next() // len
	op := p.next()
	num := p.next()
	if op.kind != tOp || num.kind != tNum {
		return 0, false, fmt.Errorf("%w: bad len comparison", errUnsupported)
	}
	n, ok := new(big.Int).SetString(num.text, 10)
	if !ok || !n.IsInt64() {
		return 0, false, fmt.Errorf("%w: non-integer len bound %q", errUnsupported, num.text)
	}
	b := n.Int64()
	switch op.text {
	case "<=":
		return b, false, nil
	case "<":
		return b - 1, false, nil
	case ">":
		return b, true, nil
	case ">=":
		return b - 1, true, nil
	}
	return 0, false, fmt.Errorf("%w: operator %q", errUnsupported, op.text)
}

// leaf := "tier" "(" STRING "," body ")"
func (p *parser) parseLeaf() (node, error) {
	if err := p.expect(tIdent, "tier"); err != nil {
		return nil, err
	}
	if err := p.expect(tOp, "("); err != nil {
		return nil, err
	}
	if t := p.next(); t.kind != tStr {
		return nil, fmt.Errorf("%w: tier name must be a string", errUnsupported)
	}
	if err := p.expect(tOp, ","); err != nil {
		return nil, err
	}
	leaf, err := p.parseBody()
	if err != nil {
		return nil, err
	}
	if err := p.expect(tOp, ")"); err != nil {
		return nil, err
	}
	return &leafNode{leaf}, nil
}

var priceVars = map[string]bool{"p": true, "c": true, "cr": true, "cc": true, "cc1h": true}

// body := "fixed" "(" NUM ")" | term ("+" term)*
// term := VAR "*" NUM | NUM "*" VAR | VAR
func (p *parser) parseBody() (*exprLeaf, error) {
	if t := p.peek(); t.kind == tIdent && t.text == "fixed" {
		p.next()
		if err := p.expect(tOp, "("); err != nil {
			return nil, err
		}
		v, err := p.number()
		if err != nil {
			return nil, err
		}
		if err := p.expect(tOp, ")"); err != nil {
			return nil, err
		}
		return &exprLeaf{fixed: v}, nil
	}
	leaf := &exprLeaf{coef: map[string]*big.Rat{}}
	for {
		name, coef, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		if _, dup := leaf.coef[name]; dup {
			return nil, fmt.Errorf("%w: variable %s appears twice", errUnsupported, name)
		}
		leaf.coef[name] = coef
		if t := p.peek(); t.kind == tOp && t.text == "+" {
			p.next()
			continue
		}
		break
	}
	if leaf.coef["p"] == nil || leaf.coef["c"] == nil {
		return nil, fmt.Errorf("%w: leaf must price both p and c", errUnsupported)
	}
	return leaf, nil
}

func (p *parser) parseTerm() (string, *big.Rat, error) {
	t := p.next()
	switch t.kind {
	case tIdent:
		if !priceVars[t.text] {
			return "", nil, fmt.Errorf("%w: variable %q", errUnsupported, t.text)
		}
		if n := p.peek(); n.kind == tOp && n.text == "*" {
			p.next()
			v, err := p.number()
			return t.text, v, err
		}
		return t.text, big.NewRat(1, 1), nil
	case tNum:
		v, ok := new(big.Rat).SetString(t.text)
		if !ok {
			return "", nil, fmt.Errorf("%w: number %q", errUnsupported, t.text)
		}
		if err := p.expect(tOp, "*"); err != nil {
			return "", nil, err
		}
		name := p.next()
		if name.kind != tIdent || !priceVars[name.text] {
			return "", nil, fmt.Errorf("%w: variable %q", errUnsupported, name.text)
		}
		return name.text, v, nil
	}
	return "", nil, fmt.Errorf("%w: unexpected %q", errUnsupported, t.text)
}

func (p *parser) number() (*big.Rat, error) {
	t := p.next()
	if t.kind != tNum {
		return nil, fmt.Errorf("%w: expected number, got %q", errUnsupported, t.text)
	}
	v, ok := new(big.Rat).SetString(t.text)
	if !ok || v.Sign() < 0 {
		return nil, fmt.Errorf("%w: number %q", errUnsupported, t.text)
	}
	return v, nil
}
