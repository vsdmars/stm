/*
package stm provides Software Transactional Memory operations for Go. This is
an alternative to the standard way of writing concurrent code (channels and
mutexes). STM makes it easy to perform arbitrarily complex operations in an
atomic fashion. One of its primary advantages over traditional locking is that
STM transactions are composable, whereas locking functions are not -- the
composition will either deadlock or release the lock between functions (making
it non-atomic).

To begin, create an STM object that wraps the data you want to access
concurrently.

	x := stm.NewVar(3)

You can then use the Atomically method to atomically read and/or write the the
data. This code atomically decrements x:

	stm.Atomically(func(tx *stm.Tx) {
		cur := tx.Get(x).(int)
		tx.Set(x, cur-1)
	})

An important part of STM transactions is retrying. At any point during the
transaction, you can call tx.Retry(), which will abort the transaction, but
not cancel it entirely. The call to Atomically will block until another call
to Atomically finishes, at which point the transaction will be rerun.
Specifically, one of the values read by the transaction (via tx.Get) must be
updated before the transaction will be rerun. As an example, this code will
try to decrement x, but will block as long as x is zero:

	stm.Atomically(func(tx *stm.Tx) {
		cur := tx.Get(x).(int)
		if cur == 0 {
			tx.Retry()
		}
		tx.Set(x, cur-1)
	})

Internally, tx.Retry simply calls panic(stm.Retry). Panicking with any other
value will cancel the transaction; no values will be changed. However, it is
the responsibility of the caller to catch such panics.

Multiple transactions can be composed using Select. If the first transaction
calls Retry, the next transaction will be run, and so on. If all of the
transactions call Retry, the call will block and the entire selection will be
retried. For example, this code implements the "decrement-if-nonzero"
transaction above, but for two values. It will first try to decrement x, then
y, and block if both values are zero.

	func dec(v *stm.Var) {
		return func(tx *stm.Tx) {
			cur := tx.Get(v).(int)
			if cur == 0 {
				tx.Retry()
			}
			tx.Set(v, cur-1)
		}
	}

	// Note that Select does not perform any work itself, but merely
	// returns a transaction function.
	stm.Atomically(stm.Select(dec(x), dec(y)))

An important caveat: transactions must not have side effects! This is because a
transaction may be restarted several times before completing, meaning the side
effects may execute more than once. This will almost certainly cause incorrect
behavior. One common way to get around this is to build up a list of operations
to perform inside the transaction, and then perform them after the transaction
completes.

The stm API tries to mimic that of Haskell's Control.Concurrent.STM, but this
is not entirely possible due to Go's type system; we are forced to use
interface{} and type assertions. Furthermore, Haskell can enforce at compile
time that STM variables are not modified outside the STM monad. This is not
possible in Go, so be especially careful when using pointers in your STM code.

It remains to be seen whether this style of concurrency has practical
applications in Go. If you find this package useful, please tell me about it!
*/
package stm

import (
	"sync"
)

// Retry is a sentinel value. When thrown via panic, it indicates that a
// transaction should be retried.
const Retry = "retry"

// The globalLock serializes transaction verification/committal.
var globalLock sync.Mutex

// A Var holds an STM variable.
type Var struct {
	val interface{}
}

// NewVar returns a new STM variable.
func NewVar(val interface{}) *Var {
	return &Var{val}
}

// A Tx represents an atomic transaction.
type Tx struct {
	reads  map[*Var]interface{}
	writes map[*Var]interface{}
}

// verify checks that none of the logged values have changed since the
// transaction began
func (tx *Tx) verify() bool {
	for v, val := range tx.reads {
		if v.val != val {
			return false
		}
	}
	return true
}

// commit writes the values in the transaction log to their respective Vars.
func (tx *Tx) commit() {
	for v, val := range tx.writes {
		v.val = val
	}
}

// Get returns the value of v as of the start of the transaction.
func (tx *Tx) Get(v *Var) interface{} {
	// If we previously wrote to v, it will be in the write log.
	if val, ok := tx.writes[v]; ok {
		return val
	}
	// If we previously read v, it will be in the read log.
	if val, ok := tx.reads[v]; ok {
		return val
	}
	// Otherwise, record and return its current value.
	globalLock.Lock()
	defer globalLock.Unlock()
	tx.reads[v] = v.val
	return v.val
}

// Set sets the value of a Var for the lifetime of the transaction.
func (tx *Tx) Set(v *Var, val interface{}) {
	tx.writes[v] = val
}

// Retry aborts the transaction and retries it when a Var changes.
func (tx *Tx) Retry() {
	panic(Retry)
}

// Assert is a helper function that retries a transaction if the condition is
// not satisfied.
func (tx *Tx) Assert(p bool) {
	if !p {
		tx.Retry()
	}
}

// catchRetry returns true if fn calls tx.Retry.
func catchRetry(fn func(*Tx), tx *Tx) (retry bool) {
	defer func() {
		if r := recover(); r == Retry {
			retry = true
		} else if r != nil {
			panic(r)
		}
	}()
	fn(tx)
	return
}

// Select runs the supplied functions in order. Execution stops when a
// function succeeds without calling Retry. If no functions succeed, the
// entire selection will be retried.
func Select(fns ...func(*Tx)) func(*Tx) {
	return func(tx *Tx) {
		switch len(fns) {
		case 0:
			// empty Select blocks forever
			tx.Retry()
		case 1:
			fns[0](tx)
		default:
			if catchRetry(fns[0], tx) {
				Select(fns[1:]...)(tx)
			}
		}
	}
}

// Atomically executes the atomic function fn.
func Atomically(fn func(*Tx)) {
retry:
	// run the transaction
	tx := &Tx{
		reads:  make(map[*Var]interface{}),
		writes: make(map[*Var]interface{}),
	}
	if catchRetry(fn, tx) {
		goto retry
	}
	// verify the read log
	globalLock.Lock()
	if !tx.verify() {
		globalLock.Unlock()
		goto retry
	}
	// commit the write log
	if len(tx.writes) > 0 {
		tx.commit()
	}
	globalLock.Unlock()
}

// AtomicGet is a helper function that atomically reads a value.
func AtomicGet(v *Var) interface{} {
	var i interface{}
	Atomically(func(tx *Tx) { i = tx.Get(v) })
	return i
}

// AtomicSet is a helper function that atomically writes a value.
func AtomicSet(v *Var, val interface{}) {
	Atomically(func(tx *Tx) { tx.Set(v, val) })
}

// Compose is a helper function that composes multiple transactions into a
// single transaction.
func Compose(fns ...func(*Tx)) func(*Tx) {
	return func(tx *Tx) {
		for _, f := range fns {
			f(tx)
		}
	}
}
