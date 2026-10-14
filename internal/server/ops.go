package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// The closed operation whitelist. This *is* the sandbox: there is deliberately
// no "run this script" operation, because that would make the executor a
// general remote-code endpoint and void the fail-closed model. Adding an
// operation is a contract change with its own review.
var knownOperations = map[string]struct{}{
	"arith":  {},
	"digest": {},
}

// ErrUnknownOperation is returned for any operation outside the whitelist.
var ErrUnknownOperation = errors.New("server: operation not in whitelist")

// Execute runs a whitelisted operation on its params. It performs no I/O, opens
// no network and touches no filesystem.
func Execute(operation, paramsJSON string) (string, error) {
	if _, ok := knownOperations[operation]; !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownOperation, operation)
	}
	switch operation {
	case "arith":
		var p struct {
			Expression string `json:"expression"`
		}
		if err := json.Unmarshal([]byte(paramsJSON), &p); err != nil {
			return "", fmt.Errorf("server: arith params: %w", err)
		}
		v, err := evalArith(p.Expression)
		if err != nil {
			return "", err
		}
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	case "digest":
		var p struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal([]byte(paramsJSON), &p); err != nil {
			return "", fmt.Errorf("server: digest params: %w", err)
		}
		sum := sha256.Sum256([]byte(p.Data))
		return hex.EncodeToString(sum[:]), nil
	}
	return "", ErrUnknownOperation
}

// --- restricted arithmetic evaluator -----------------------------------------
//
// Supports + - * / // % **, unary minus, parentheses and numeric literals.
// Names, calls, attributes, subscripts, comprehensions and lambdas are not in
// the grammar, so they cannot be injected.

func evalArith(expr string) (float64, error) {
	p := &parser{src: expr}
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return 0, fmt.Errorf("server: unexpected %q at %d", p.src[p.pos], p.pos)
	}
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, errors.New("server: arithmetic result is not finite")
	}
	return v, nil
}

type parser struct {
	src string
	pos int
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *parser) peek() byte {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) parseExpr() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '+':
			p.pos++
			r, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			left += r
		case '-':
			p.pos++
			r, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			left -= r
		default:
			return left, nil
		}
	}
}

func (p *parser) parseTerm() (float64, error) {
	left, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '*':
			p.pos++
			if p.peek() == '*' {
				p.pos++
				r, err := p.parseUnary()
				if err != nil {
					return 0, err
				}
				left = math.Pow(left, r)
				continue
			}
			r, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			left *= r
		case '/':
			p.pos++
			if p.peek() == '/' {
				p.pos++
				r, err := p.parseUnary()
				if err != nil {
					return 0, err
				}
				if r == 0 {
					return 0, errors.New("server: division by zero")
				}
				left = math.Floor(left / r)
				continue
			}
			r, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, errors.New("server: division by zero")
			}
			left /= r
		case '%':
			p.pos++
			r, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, errors.New("server: modulo by zero")
			}
			left = math.Mod(left, r)
		default:
			return left, nil
		}
	}
}

func (p *parser) parseUnary() (float64, error) {
	switch p.peek() {
	case '-':
		p.pos++
		v, err := p.parseUnary()
		return -v, err
	case '+':
		p.pos++
		return p.parseUnary()
	case '(':
		p.pos++
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if p.peek() != ')' {
			return 0, errors.New("server: missing closing parenthesis")
		}
		p.pos++
		return v, nil
	default:
		return p.parseNumber()
	}
}

func (p *parser) parseNumber() (float64, error) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= '0' && c <= '9') || c == '.' {
			p.pos++
			continue
		}
		break
	}
	if start == p.pos {
		return 0, fmt.Errorf("server: expected a number at %d", start)
	}
	lit := p.src[start:p.pos]
	v, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return 0, fmt.Errorf("server: bad number %q: %w", lit, err)
	}
	return v, nil
}

// OperationNames returns the whitelist for display.
func OperationNames() []string {
	out := make([]string, 0, len(knownOperations))
	for k := range knownOperations {
		out = append(out, k)
	}
	return out
}
