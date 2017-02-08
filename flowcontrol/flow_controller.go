package flowcontrol

import (
	"errors"
	"time"

	"github.com/lucas-clemente/quic-go/congestion"
	"github.com/lucas-clemente/quic-go/handshake"
	"github.com/lucas-clemente/quic-go/protocol"
	"github.com/lucas-clemente/quic-go/utils"
)

type flowController struct {
	streamID protocol.StreamID

	connectionParameters handshake.ConnectionParametersManager
	rttStats             *congestion.RTTStats

	bytesSent  protocol.ByteCount
	sendWindow protocol.ByteCount

	lastWindowUpdateTime time.Time

	bytesRead                 protocol.ByteCount
	highestReceived           protocol.ByteCount
	receiveWindow             protocol.ByteCount
	receiveWindowIncrement    protocol.ByteCount
	maxReceiveWindowIncrement protocol.ByteCount
}

// ErrReceivedSmallerByteOffset occurs if the ByteOffset received is smaller than a ByteOffset that was set previously
var ErrReceivedSmallerByteOffset = errors.New("Received a smaller byte offset")

// newFlowController gets a new flow controller
func newFlowController(streamID protocol.StreamID, connectionParameters handshake.ConnectionParametersManager, rttStats *congestion.RTTStats) *flowController {
	fc := flowController{
		streamID:             streamID,
		connectionParameters: connectionParameters,
		rttStats:             rttStats,
	}

	if streamID == 0 {
		fc.receiveWindow = connectionParameters.GetReceiveConnectionFlowControlWindow()
		fc.receiveWindowIncrement = fc.receiveWindow
		fc.maxReceiveWindowIncrement = connectionParameters.GetMaxReceiveConnectionFlowControlWindow()
	} else {
		fc.receiveWindow = connectionParameters.GetReceiveStreamFlowControlWindow()
		fc.receiveWindowIncrement = fc.receiveWindow
		fc.maxReceiveWindowIncrement = connectionParameters.GetMaxReceiveStreamFlowControlWindow()
	}

	return &fc
}

func (c *flowController) getSendWindow() protocol.ByteCount {
	if c.sendWindow == 0 {
		if c.streamID == 0 {
			return c.connectionParameters.GetSendConnectionFlowControlWindow()
		}
		return c.connectionParameters.GetSendStreamFlowControlWindow()
	}
	return c.sendWindow
}

func (c *flowController) AddBytesSent(n protocol.ByteCount) {
	c.bytesSent += n
}

// UpdateSendWindow should be called after receiving a WindowUpdateFrame
// it returns true if the window was actually updated
func (c *flowController) UpdateSendWindow(newOffset protocol.ByteCount) bool {
	if newOffset > c.sendWindow {
		c.sendWindow = newOffset
		return true
	}
	return false
}

func (c *flowController) SendWindowSize() protocol.ByteCount {
	sendWindow := c.getSendWindow()

	if c.bytesSent > sendWindow { // should never happen, but make sure we don't do an underflow here
		return 0
	}
	return sendWindow - c.bytesSent
}

func (c *flowController) SendWindowOffset() protocol.ByteCount {
	return c.getSendWindow()
}

// UpdateHighestReceived updates the highestReceived value, if the byteOffset is higher
// Should **only** be used for the stream-level FlowController
// it returns an ErrReceivedSmallerByteOffset if the received byteOffset is smaller than any byteOffset received before
// This error occurs every time StreamFrames get reordered and has to be ignored in that case
// It should only be treated as an error when resetting a stream
func (c *flowController) UpdateHighestReceived(byteOffset protocol.ByteCount) (protocol.ByteCount, error) {
	if byteOffset == c.highestReceived {
		return 0, nil
	}
	if byteOffset > c.highestReceived {
		increment := byteOffset - c.highestReceived
		c.highestReceived = byteOffset
		return increment, nil
	}
	return 0, ErrReceivedSmallerByteOffset
}

// IncrementHighestReceived adds an increment to the highestReceived value
// Should **only** be used for the connection-level FlowController
func (c *flowController) IncrementHighestReceived(increment protocol.ByteCount) {
	c.highestReceived += increment
}

func (c *flowController) AddBytesRead(n protocol.ByteCount) {
	c.bytesRead += n
}

// MaybeUpdateWindow determines if it is necessary to send a WindowUpdate
// if so, it returns true and the offset of the window
func (c *flowController) MaybeUpdateWindow() (bool, protocol.ByteCount) {
	diff := c.receiveWindow - c.bytesRead

	// Chromium implements the same threshold
	if diff < (c.receiveWindowIncrement / 2) {
		c.maybeAdjustWindowIncrement()
		c.lastWindowUpdateTime = time.Now()
		c.receiveWindow = c.bytesRead + c.receiveWindowIncrement
		return true, c.receiveWindow
	}

	return false, 0
}

// maybeAdjustWindowIncrement increases the receiveWindowIncrement if we're sending WindowUpdates too often
func (c *flowController) maybeAdjustWindowIncrement() {
	if c.lastWindowUpdateTime.IsZero() {
		return
	}

	rtt := c.rttStats.SmoothedRTT()
	if rtt == 0 {
		return
	}

	timeSinceLastWindowUpdate := time.Now().Sub(c.lastWindowUpdateTime)

	// interval between the window updates is sufficiently large, no need to increase the increment
	if timeSinceLastWindowUpdate >= 2*rtt {
		return
	}

	oldWindowSize := c.receiveWindowIncrement
	c.receiveWindowIncrement = utils.MinByteCount(2*c.receiveWindowIncrement, c.maxReceiveWindowIncrement)

	// debug log, if the window size was actually increased
	if oldWindowSize < c.receiveWindowIncrement {
		newWindowSize := c.receiveWindowIncrement / (1 << 10)
		if c.streamID == 0 {
			utils.Debugf("Increasing receive flow control window for the connection to %d kB", newWindowSize)
		} else {
			utils.Debugf("Increasing receive flow control window increment for stream %d to %d kB", c.streamID, newWindowSize)
		}
	}
}

func (c *flowController) CheckFlowControlViolation() bool {
	if c.highestReceived > c.receiveWindow {
		return true
	}
	return false
}
