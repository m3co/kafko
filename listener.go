package kafko

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/segmentio/kafka-go"
)

type ProcessDroppedMsgHandler func(msg *kafka.Message, log Logger) error

type Logger interface {
	Printf(format string, v ...any)
	Panicf(err error, format string, v ...any)
	Errorf(err error, format string, v ...any)
}

type Reader interface {
	Close() error
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
}

var (
	ErrMessageDropped     = errors.New("message dropped")
	ErrResourceIsNil      = errors.New("resource is nil")
	ErrExitProcessingLoop = errors.New("listener: exit processing loop")
)

type Listener struct {
	messageChan    chan []byte
	errorChan      chan error
	shuttingDownCh chan struct{}

	log Logger

	recommitTicker    *time.Ticker
	processingTimeout time.Duration
	reconnectInterval time.Duration
	processDroppedMsg ProcessDroppedMsgHandler

	readerFactory ReaderFactory
	reader        Reader

	uncommittedMsgs      []kafka.Message
	uncommittedMsgsMutex *sync.Mutex

	metricMessagesProcessed Incrementer
	metricMessagesDropped   Incrementer
	metricKafkaErrors       Incrementer
}

// processError handles errors in processing messages.
func (listener *Listener) processError(ctx context.Context, message kafka.Message) error {
	select {
	case err := <-listener.errorChan:
		// If there's an error, log it and continue processing.
		if err != nil {
			listener.log.Errorf(err, "Failed to process message =%v", message)

			return nil
		}

		// If there's no error, commit the message.
		if err := listener.doCommitMessage(ctx, message); err != nil {
			return errors.Wrap(err, "err := queue.doCommitMessage(ctx, message)")
		}

	case <-time.After(listener.processingTimeout):
		// If processing times out, attempt to process the dropped message.
		if err := listener.processDroppedMsg(&message, listener.log); err != nil {
			listener.log.Errorf(err, "Failed to process message")
		}
	}

	return nil
}

// processMessageAndError processes the given message and handles any errors
// that occur during processing, following a similar approach to processError.
func (listener *Listener) processMessageAndError(ctx context.Context, message kafka.Message) error {
	select {
	case listener.messageChan <- message.Value:
		// Process the message and handle any errors.
		if err := listener.processError(ctx, message); err != nil {
			return errors.Wrap(err, "err := listener.processError(ctx, message)")
		}

	case <-time.After(listener.processingTimeout):
		// Attempt to empty the listener.lastMsg channel if there is a message.
		select {
		case _, closed := <-listener.messageChan:
			if closed {
				// If the listener.messageChan has been closed, exit the loop.
				return nil
			}
		default:
		}

		go listener.metricMessagesDropped.Inc()

		// If processing times out, attempt to process the dropped message.
		if err := listener.processDroppedMsg(&message, listener.log); err != nil {
			listener.log.Errorf(err, "Failed to process message")
		}
	}

	return nil
}

// addUncommittedMsg appends the given message to the list of uncommitted messages.
// It locks the uncommittedMsgsMutex to ensure safe concurrent access to the uncommittedMsgs slice.
func (listener *Listener) addUncommittedMsg(message kafka.Message) {
	// Lock the mutex before accessing uncommittedMsgs.
	listener.uncommittedMsgsMutex.Lock()

	// Unlock the mutex after finishing.
	defer listener.uncommittedMsgsMutex.Unlock()

	// Add the message to the uncommittedMsgs slice.
	listener.uncommittedMsgs = append(listener.uncommittedMsgs, message)
}

// doCommitMessage adds the given message to the list of uncommitted messages
// and commits all uncommitted messages.
func (listener *Listener) doCommitMessage(ctx context.Context, message kafka.Message) error {
	// Add the message to the list of uncommitted messages.
	listener.addUncommittedMsg(message)

	// Attempt to commit all uncommitted messages.
	if err := listener.commitUncommittedMessages(ctx); err != nil {
		// If there's an error, handle it and return the wrapped error.
		if err := listener.handleKafkaError(ctx, err); err != nil {
			return errors.Wrap(err, "err := queue.reader.CommitMessages(ctx, queue.uncommittedMsgs)")
		}
	}

	return nil
}

// handleKafkaError checks if the error is temporary or a timeout and
// takes appropriate action based on the error type. If the error is recoverable,
// it attempts to reconnect to Kafka. If the error is not recoverable, it wraps and returns the error.
func (listener *Listener) handleKafkaError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}

	var kafkaError *kafka.Error

	if errors.As(err, &kafkaError) {
		if kafkaError.Temporary() || kafkaError.Timeout() {
			listener.log.Printf("Kafka error, but this is a recoverable error so let's retry. Reason = %v", err)

			select {
			// Let's reconnect after queue.reconnectInterval.
			case <-time.After(listener.reconnectInterval):
				listener.reconnectToKafka()

			// If ctx.Done and reconnect hasn't started yet, then it's secure to exit.
			case <-ctx.Done():
				if err := ctx.Err(); err != nil {
					return errors.Wrap(err, "err := ctx.Err() (ctx.Done()) (handleKafkaError)")
				}

			// If the shutdown has started, exit the loop.
			case <-listener.shuttingDownCh:
				return ErrExitProcessingLoop
			}

			// Return no error since it's a recoverable error
			return nil
		}
	}

	// If the error is not recoverable, wrap and return it.
	return errors.Wrapf(err, "Failed to commit message, unrecoverable error")
}

// commitUncommittedMessages commits all uncommitted messages to Kafka.
// It locks the uncommittedMsgsMutex to avoid concurrent access to uncommittedMsgs.
func (listener *Listener) commitUncommittedMessages(ctx context.Context) error {
	// Lock the mutex to avoid concurrent access to uncommitted messages.
	listener.uncommittedMsgsMutex.Lock()
	defer listener.uncommittedMsgsMutex.Unlock()

	// If there are uncommitted messages, attempt to commit them.
	if len(listener.uncommittedMsgs) > 0 {
		if err := listener.reader.CommitMessages(ctx, listener.uncommittedMsgs...); err != nil {
			go listener.metricKafkaErrors.Inc()

			return errors.Wrapf(err, "err := queue.reader.CommitMessages(ctx, queue.uncommittedMsgs...) (queue.uncommittedMsgs = %v)", listener.uncommittedMsgs)
		}

		go listener.metricMessagesProcessed.Inc()

		// Reset the uncommitted messages slice.
		listener.uncommittedMsgs = nil
	}

	return nil
}

// runCommitLoop is a method of the Listener struct that handles periodic committing of uncommitted messages.
// It is designed to be run in a separate goroutine and will continue until the provided context is cancelled or completed.
//
// The method uses a ticker to trigger periodic commits and makes use of a defer function to ensure proper cleanup
// in case of a panic or other unexpected situations. The defer function stops the ticker and attempts to commit any
// remaining uncommitted messages.
//
// This method is part of a message processing system and is typically used in conjunction with other methods that handle
// message reception and processing.
func (listener *Listener) runCommitLoop(ctx context.Context) {
	// Add the defer function to handle stopping the ticker and committing uncommitted messages
	// in case the method returns due to a panic or other unexpected situations.
	defer func() {
		listener.recommitTicker.Stop()

		if err := listener.commitUncommittedMessages(ctx); err != nil {
			listener.log.Errorf(err, "err := queue.commitUncommittedMessages(ctx)")
		}
	}()

	// Loop until the context is done.
	for {
		select {
		case <-listener.recommitTicker.C:
			// When the ticker ticks, commit uncommitted messages.
			if err := listener.commitUncommittedMessages(ctx); err != nil {
				listener.log.Errorf(err, "err := queue.commitUncommittedMessages(ctx)")
			}

		case <-listener.shuttingDownCh:
			// If the shutdown has started, exit the loop.
			return

		case <-ctx.Done():
			// If the context is done, exit the loop.
			return
		}
	}
}

// reconnectToKafka attempts to reconnect the Listener to the Kafka broker.
// It returns an error if the connection fails.
func (listener *Listener) reconnectToKafka() {
	// Close the existing reader in order to avoid resource leaks
	if err := listener.reader.Close(); err != nil {
		go listener.metricKafkaErrors.Inc()

		listener.log.Errorf(err, "err := listener.reader.Close()")
	}

	// Create a new Reader from the readerFactory.
	reader := listener.readerFactory()
	listener.reader = reader
}

// MessageAndErrorChannels returns the message and error channels for the Listener.
func (listener *Listener) MessageAndErrorChannels() (<-chan []byte, chan<- error) {
	return listener.messageChan, listener.errorChan
}

// Shutdown gracefully shuts down the Listener, committing any uncommitted messages
// and closing the Kafka reader.
func (listener *Listener) Shutdown(ctx context.Context) error {
	// let's start the shutting down process
	close(listener.shuttingDownCh)

	defer func() {
		close(listener.errorChan)
		close(listener.messageChan)
	}()

	// Commit any uncommitted messages. It's OK to not to process them further as
	// logs will provide the missing content while trying to commit before shutting down.
	if err := listener.commitUncommittedMessages(ctx); err != nil {
		listener.log.Errorf(err, "err := queue.commitUncommittedMessages(ctx)")
	}

	// Close the Kafka reader.
	if err := listener.reader.Close(); err != nil {
		go listener.metricKafkaErrors.Inc()

		return errors.Wrap(err, "queue.reader.Close()")
	}

	return nil
}

func (listener *Listener) processTick(ctx context.Context) error {
	// Fetch a message from the Kafka topic.
	message, err := listener.reader.FetchMessage(ctx)

	// If there's an error, handle the message error and continue to the next iteration.
	if err != nil {
		go listener.metricKafkaErrors.Inc()

		if err := listener.handleKafkaError(ctx, err); err != nil {
			return errors.Wrap(err, "err := listener.handleKafkaError(ctx, err)")
		}

		return nil
	}

	// Process the message and handle any errors.
	if err := listener.processMessageAndError(ctx, message); err != nil {
		return errors.Wrap(err, "err := listener.processMessage(ctx, message)")
	}

	return nil
}

// Listen starts the Listener to fetch and process messages from the Kafka topic.
// It also starts the commit loop and handles message errors.
func (listener *Listener) Listen(ctx context.Context) error { //nolint:cyclop
	// Start the commit loop in a separate goroutine.
	go listener.runCommitLoop(ctx)

	// Continuously fetch and process messages.
	for {
		select {
		case _, isOpen := <-listener.messageChan:
			closed := !isOpen
			if closed {
				// If the listener.messageChan has been closed, exit the loop.
				return nil
			}

		case <-ctx.Done():
			// If the context is done, check for an error and return it.
			if err := ctx.Err(); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}

				return errors.Wrap(err, "err := ctx.Err() (ctx.Done()) (Listen)")
			}

		case <-listener.shuttingDownCh:
			// If the shutdown has started, exit the loop.
			return nil

		default:
		}

		err := listener.processTick(ctx)

		if errors.Is(err, ErrExitProcessingLoop) {
			return nil
		}

		if errors.Is(err, context.Canceled) {
			return nil
		}

		if err != nil {
			return errors.Wrap(err, "err := listener.processTick(ctx)")
		}
	}
}

// NewListener creates a new Listener instance with the provided configuration,
// logger, and optional custom options.
func NewListener(log Logger, opts ...*Options) *Listener {
	finalOpts := obtainFinalOpts(log, opts)

	// messageChan should have a buffer size of 1 to accommodate for the case when
	// the consumer did not process the message within the `processingTimeout` period.
	// In the Listen method, we attempt to empty the listener.messageChan channel (only once)
	// if the processingTimeout is reached. By setting the buffer size to 1, we ensure
	// that the new message can be placed in the channel even if the previous message
	// wasn't processed within the given timeout.
	messageChan := make(chan []byte, 1)

	// errorChan has a buffer size of 1 to allow the sender to send an error without blocking
	// if the receiver is not ready to receive it yet.
	errorChan := make(chan error, 1)

	shuttingDownCh := make(chan struct{}, 1)

	// Create and return a new Listener instance with the final configuration,
	// channels, and options.
	return &Listener{
		messageChan:    messageChan,
		errorChan:      errorChan,
		shuttingDownCh: shuttingDownCh,

		log:           log,
		readerFactory: finalOpts.readerFactory,
		reader:        finalOpts.readerFactory(),

		recommitTicker:    time.NewTicker(finalOpts.recommitInterval),
		reconnectInterval: finalOpts.reconnectInterval,
		processingTimeout: finalOpts.processingTimeout,
		processDroppedMsg: finalOpts.processDroppedMsg,

		uncommittedMsgsMutex: &sync.Mutex{},

		metricMessagesProcessed: finalOpts.metricMessagesProcessed,
		metricMessagesDropped:   finalOpts.metricMessagesDropped,
		metricKafkaErrors:       finalOpts.metricKafkaErrors,
	}
}
