package blaster

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"

	"time"

	"sync/atomic"

	"github.com/leemcloughlin/gofarmhash"
	"github.com/pkg/errors"
	"github.com/spf13/viper"
)

const DEBUG = false

type Blaster struct {
	config          *configDef
	viper           *viper.Viper
	rate            float64
	softTimeout     time.Duration
	hardTimeout     time.Duration
	skip            map[farmhash.Uint128]struct{}
	dataCloser      io.Closer
	dataReader      DataReader
	dataHeaders     []string
	logCloser       io.Closer
	logWriter       LogWriteFlusher
	cancel          context.CancelFunc
	out             io.Writer
	rateInputReader io.Reader

	mainChannel            chan struct{}
	errorChannel           chan error
	workerChannel          chan workDef
	logChannel             chan logRecord
	dataFinishedChannel    chan struct{}
	workersFinishedChannel chan struct{}
	changeRateChannel      chan float64
	signalChannel          chan os.Signal

	mainWait   *sync.WaitGroup
	workerWait *sync.WaitGroup

	workerTypes map[string]func() Worker

	errorsIgnored uint64
	metrics       *metricsDef
	err           error
}

type DataReader interface {
	Read() (record []string, err error)
}

type LogWriteFlusher interface {
	Write(record []string) error
	Flush()
}

func New(ctx context.Context, cancel context.CancelFunc) *Blaster {

	b := &Blaster{
		viper:                  viper.New(),
		cancel:                 cancel,
		mainWait:               new(sync.WaitGroup),
		workerWait:             new(sync.WaitGroup),
		workerTypes:            make(map[string]func() Worker),
		skip:                   make(map[farmhash.Uint128]struct{}),
		dataFinishedChannel:    make(chan struct{}),
		workersFinishedChannel: make(chan struct{}),
		changeRateChannel:      make(chan float64, 1),
		errorChannel:           make(chan error),
		logChannel:             make(chan logRecord),
		mainChannel:            make(chan struct{}),
		workerChannel:          make(chan workDef),
	}
	b.metrics = newMetricsDef(b)

	// trap Ctrl+C and call cancel on the context
	b.signalChannel = make(chan os.Signal, 1)
	signal.Notify(b.signalChannel, os.Interrupt)
	go func() {
		select {
		case <-b.signalChannel:
			b.cancel()
		case <-ctx.Done():
		}
	}()

	return b
}

func (b *Blaster) Exit() {
	signal.Stop(b.signalChannel)
	b.cancel()
}

func (b *Blaster) Start(ctx context.Context) error {

	// os.Stdout isn't guaranteed to be safe for concurrent access
	b.out = NewThreadSafeWriter(os.Stdout)

	if err := b.loadConfigViper(); err != nil {
		return err
	}

	if b.config.Data == "" {
		return errors.New("No data file specified. Use --config to view current config.")
	}

	headers, err := b.openDataFile(ctx)
	if err != nil {
		return err
	}
	b.dataHeaders = headers
	defer b.closeDataFile()

	if err := b.openLogAndInit(); err != nil {
		return err
	}
	defer b.flushAndCloseLog()

	b.rateInputReader = os.Stdin

	return b.start(ctx)
}

func (b *Blaster) start(ctx context.Context) error {

	b.metrics.addSegment(b.rate)

	b.startTickerLoop(ctx)
	b.startMainLoop(ctx)
	b.startErrorLoop(ctx)
	b.startWorkers(ctx)
	b.startLogLoop(ctx)
	b.startStatusLoop(ctx)
	b.startRateLoop(ctx)

	b.printRatePrompt()

	// wait for cancel or finished
	select {
	case <-ctx.Done():
	case <-b.dataFinishedChannel:
	}

	fmt.Fprintln(b.out, "Waiting for workers to finish...")
	b.workerWait.Wait()
	fmt.Fprintln(b.out, "All workers finished.")

	// signal to log and error loop that it's tine to exit
	close(b.workersFinishedChannel)

	fmt.Fprintln(b.out, "Waiting for processes to finish...")
	b.mainWait.Wait()
	fmt.Fprintln(b.out, "All processes finished.")

	if b.err != nil {
		fmt.Fprintln(b.out, "")
		errorsIgnored := atomic.LoadUint64(&b.errorsIgnored)
		if errorsIgnored > 0 {
			fmt.Fprintf(b.out, "%d errors were ignored because we were already exiting with an error.\n", errorsIgnored)
		}
		fmt.Fprintf(b.out, "Fatal error: %v\n", b.err)
	} else {
		b.printStatus(true)
	}

	return nil
}

func (b *Blaster) RegisterWorkerType(key string, workerFunc func() Worker) {
	b.workerTypes[key] = workerFunc
}

type Worker interface {
	Send(ctx context.Context, payload map[string]interface{}) (response map[string]interface{}, err error)
}

type Starter interface {
	Start(ctx context.Context, payload map[string]interface{}) error
}

type Stopper interface {
	Stop(ctx context.Context, payload map[string]interface{}) error
}

func NewThreadSafeWriter(w io.Writer) *ThreadSafeWriter {
	return &ThreadSafeWriter{
		w: w,
	}
}

type ThreadSafeWriter struct {
	w io.Writer
	m sync.Mutex
}

func (t *ThreadSafeWriter) Write(p []byte) (n int, err error) {
	t.m.Lock()
	defer t.m.Unlock()
	return t.w.Write(p)
}