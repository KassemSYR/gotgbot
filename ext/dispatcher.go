package ext

import (
	"encoding/json"
	"errors"
	"log"
	"runtime/debug"
	"sort"
	"sync"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

const DefaultMaxRoutines = 50

type Dispatcher struct {
	// Error handles any errors that occur during handler execution.
	Error func(ctx *Context, err error)
	// Panic handles any panics that occur during handler execution.
	// If this field is nil, the stack is logged to stderr.
	Panic func(ctx *Context, stack []byte)
	// ErrorLog is the output to log to in the case of a library error.
	ErrorLog *log.Logger

	// handlerGroups represents the list of available handler groups, numerically sorted.
	handlerGroups []int
	// handlers represents all available handles, split into groups (see handlerGroups).
	handlers map[int][]Handler

	// updatesChan is the channel that the dispatcher receives all new updates on.
	updatesChan chan json.RawMessage
	// limiter is how we limit the maximum number of goroutines for handling updates.
	limiter chan struct{}
	// waitGroup handles the number of running operations to allow for clean shutdowns.
	waitGroup sync.WaitGroup
}

type DispatcherOpts struct {
	// Error handles any errors that occur during handler execution.
	Error func(ctx *Context, err error)
	// Panic handles any panics that occur during handler execution.
	// If no panic is defined, the stack is logged to
	Panic func(ctx *Context, stack []byte)

	// MaxRoutines is used to decide how to limit the number of goroutines spawned by the dispatcher.
	// If MaxRoutines == 0, DefaultMaxRoutines is used instead.
	// If MaxRoutines < 0, no limits are imposed.
	MaxRoutines int
	ErrorLog    *log.Logger
}

// NewDispatcher creates a new dispatcher, which process and handles incoming updates from the updates channel.
func NewDispatcher(updates chan json.RawMessage, opts *DispatcherOpts) *Dispatcher {
	var limiter chan struct{}

	var errFunc func(ctx *Context, err error)
	var panicFunc func(ctx *Context, stack []byte)

	maxRoutines := DefaultMaxRoutines
	errLog := errorLog

	if opts != nil {
		if opts.MaxRoutines != 0 {
			maxRoutines = opts.MaxRoutines
		}

		if opts.ErrorLog != nil {
			errLog = opts.ErrorLog
		}

		errFunc = opts.Error
		panicFunc = opts.Panic
	}

	if maxRoutines >= 0 {
		if maxRoutines == 0 {
			maxRoutines = DefaultMaxRoutines
		}

		limiter = make(chan struct{}, maxRoutines)
	}

	return &Dispatcher{
		Error:       errFunc,
		Panic:       panicFunc,
		ErrorLog:    errLog,
		updatesChan: updates,
		handlers:    make(map[int][]Handler),
		limiter:     limiter,
		waitGroup:   sync.WaitGroup{},
	}
}

// Start to handle incoming updates
func (d *Dispatcher) Start(b *gotgbot.Bot) {
	if d.limiter == nil {
		d.limitlessDispatcher(b)
		return
	}

	d.limitedDispatcher(b)
}

// Stop waits for all currently processing updates to finish, and then returns.
func (d *Dispatcher) Stop() {
	d.waitGroup.Wait()
}

func (d *Dispatcher) limitedDispatcher(b *gotgbot.Bot) {
	for upd := range d.updatesChan {
		d.waitGroup.Add(1)

		// Send empty data to limiter.
		// if limiter buffer is full, it blocks until another update finishes processing.
		d.limiter <- struct{}{}
		go func(upd json.RawMessage) {
			d.ProcessRawUpdate(b, upd)

			<-d.limiter
			d.waitGroup.Done()
		}(upd)
	}
}

func (d *Dispatcher) limitlessDispatcher(b *gotgbot.Bot) {
	for upd := range d.updatesChan {
		d.waitGroup.Add(1)

		go func(upd json.RawMessage) {
			d.ProcessRawUpdate(b, upd)
			d.waitGroup.Done()
		}(upd)
	}
}

// AddHandler adds a new handler to the dispatcher. The dispatcher will call CheckUpdate() to see whether the handler
// should be executed, and then HandleUpdate() to execute it.
func (d *Dispatcher) AddHandler(handler Handler) {
	d.AddHandlerToGroup(handler, 0)
}

// AddHandlerToGroup adds a handler to a specific group; lowest number will be processed first.
func (d *Dispatcher) AddHandlerToGroup(handler Handler, group int) {
	currHandlers, ok := d.handlers[group]
	if !ok {
		d.handlerGroups = append(d.handlerGroups, group)
		sort.Ints(d.handlerGroups)
	}
	d.handlers[group] = append(currHandlers, handler)
}

var EndGroups = errors.New("group iteration ended")
var ContinueGroups = errors.New("group iteration continued")

func (d *Dispatcher) ProcessRawUpdate(b *gotgbot.Bot, r json.RawMessage) {
	var upd gotgbot.Update
	if err := json.Unmarshal(r, &upd); err != nil {
		d.ErrorLog.Println("failed to process raw update: " + err.Error())
		return
	}

	d.ProcessUpdate(b, &upd)
}

func (d *Dispatcher) ProcessUpdate(b *gotgbot.Bot, update *gotgbot.Update) {
	var ctx *Context

	defer func() {
		if r := recover(); r != nil {
			if d.Panic != nil {
				d.Panic(ctx, debug.Stack())
				return
			}

			d.ErrorLog.Println(debug.Stack())
		}
	}()

	for _, groupNum := range d.handlerGroups {
		for _, handler := range d.handlers[groupNum] {
			if !handler.CheckUpdate(b, update) {
				continue
			}

			if ctx == nil {
				ctx = NewContext(b, update)
			}

			err := handler.HandleUpdate(ctx)
			if err != nil {
				switch err {
				case EndGroups:
					return
				case ContinueGroups:
					continue
				default:
					if d.Error != nil {
						d.Error(ctx, err)
					}
				}
			}
			break // move to next group
		}
	}

	return
}
