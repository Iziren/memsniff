package redis

import (
	"errors"
	"strconv"

	"github.com/box/memsniff/assembly/reader"
)

const (
	tagStatus = '+'
	tagError  = '-'
	tagInt    = ':'
	tagBulk   = '$'
	tagArray  = '*'

	stackLimit = 8
)

var (
	ProtocolErr       = errors.New("RESP protocol error")
	RecursionLimitErr = errors.New("too many nested RESP arrays")
)

type ParserOptions struct {
	BulkCaptureLimit int
}

// RespParser implements a stack machine to support RESP's potentially infinite
// nested arrays.
//
// When there is insufficient data to make progress, the stack is left in its
// current state and execution is resumed by calling run on the top stack frame
// when new data is available from the network. Similarly, whenever the state of
// reading data from the Reader changes, the state of the stack must change by
// pushing or popping frames.
//
// When a stack frame finishes execution it records its result in the prior stack
// frame.  The first frame of the stack is the "root" of the parse tree, and is a
// dummy frame to store the final parse result, and has no associated logic.  The
// parse is complete when stack contains only this root frame.
type RespParser struct {
	stack   []stackFrame
	Options ParserOptions
}

// stackFrame holds a resumable piece of execution state.
type stackFrame struct {
	run    func() error
	result interface{}
}

// NewParser creates a parser ready to read a single RESP value from r.
func NewParser(r *reader.Reader) *RespParser {
	p := &RespParser{
		// start with root frame to contain eventual result
		stack: []stackFrame{{}},
	}
	p.Reset(r)
	return p
}

// Reset discards all current state and prepares the parser to read a
// single RESP value from r.
func (p *RespParser) Reset(r *reader.Reader) {
	p.stack = p.stack[:1]
	p.startParseValue(r)
}

func (p *RespParser) Run() error {
	for {
		if len(p.stack) == 1 {
			return nil
		}
		err := p.stack[len(p.stack)-1].run()
		if err != nil {
			return err
		}
	}
}

func (p *RespParser) Result() interface{} {
	return p.stack[len(p.stack)-1].result
}

func (p *RespParser) BulkArray() [][]byte {
	res := p.Result().([]interface{})
	out := make([][]byte, len(res))
	for i, b := range res {
		// just set as nil if the item was bigger than BulkCaptureLimit
		out[i], _ = b.([]byte)
	}
	return out
}

func (p *RespParser) push(f func() error) {
	p.stack = append(p.stack, stackFrame{run: f})
}

// pop removes the top frame from the stack, and places result in the previous
// frame.
func (p *RespParser) pop(result interface{}) {
	p.stack = p.stack[:len(p.stack)-1]
	p.stack[len(p.stack)-1].result = result
}

func (p *RespParser) startParseValue(r *reader.Reader) {
	p.push(func() error {
		if len(p.stack) > stackLimit {
			return RecursionLimitErr
		}
		out, err := r.ReadN(1)
		if err != nil {
			return err
		}
		p.pop(nil)
		switch out[0] {
		case tagStatus:
			p.startParseSimpleString(r, false)
		case tagError:
			p.startParseSimpleString(r, true)
		case tagInt:
			p.startParseInt(r)
		case tagBulk:
			p.startParseBulk(r)
		case tagArray:
			p.startParseArray(r)
		default:
			return ProtocolErr
		}
		return nil
	})
}

func (p *RespParser) startParseSimpleString(r *reader.Reader, asError bool) {
	p.push(func() error {
		out, err := r.ReadLine()
		if err != nil {
			return err
		}
		if asError {
			p.pop(errors.New(string(out)))
		} else {
			p.pop(string(out))
		}
		return nil
	})
}

func (p *RespParser) startParseInt(r *reader.Reader) {
	p.push(func() error {
		out, err := r.ReadLine()
		if err != nil {
			return err
		}
		i, err := strconv.Atoi(string(out))
		if err != nil {
			return err
		}
		p.pop(i)
		return nil
	})
}

func (p *RespParser) startParseBulk(r *reader.Reader) {
	// prepare handler to read and discard the body
	p.push(func() error {
		result := p.Result().(int)
		if result < 0 {
			// Redis "nil" result
			p.pop(nil)
			return nil
		}
		if result <= p.Options.BulkCaptureLimit {
			p.pop(nil)
			p.startParseBulkN(r, make([]byte, 0, result), result)
		} else {
			p.pop(result)
			r.Discard(p.Result().(int) + 2)
		}
		return nil
	})
	p.startParseInt(r)
}

func (p *RespParser) startParseBulkN(r *reader.Reader, accum []byte, n int) {
	p.push(func() error {
		out, err := r.ReadN(n)
		if err != nil {
			if err == reader.ErrShortRead {
				accum = append(accum, out...)
				p.pop(nil)
				r.Discard(len(out))
				p.startParseBulkN(r, accum, n-len(out))
			}
			return err
		}
		r.Discard(2)
		p.pop(append(accum, out...))
		return nil
	})
}

func (p *RespParser) startParseArray(r *reader.Reader) {
	p.push(func() error {
		n := p.Result().(int)
		p.pop(nil)
		p.stack[len(p.stack)-1].result = make([]interface{}, 0, n)
		p.startParseNArrayFields(r, n)
		return nil
	})
	p.startParseInt(r)
}

func (p *RespParser) startParseNArrayFields(r *reader.Reader, n int) {
	p.push(func() error {
		// value parsed
		result := p.Result()
		results := append(p.stack[len(p.stack)-2].result.([]interface{}), result)
		p.pop(results)
		if n > 1 {
			p.startParseNArrayFields(r, n-1)
			return nil
		}
		return nil
	})
	p.startParseValue(r)
}
