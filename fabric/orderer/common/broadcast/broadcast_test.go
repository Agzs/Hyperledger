/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

                 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package broadcast

import (
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/hyperledger/fabric/orderer/common/msgprocessor"
	cb "github.com/hyperledger/fabric/protos/common"
	ab "github.com/hyperledger/fabric/protos/orderer"

	logging "github.com/op/go-logging"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
)

func init() {
	logging.SetLevel(logging.DEBUG, "")
}

type mockB struct {
	grpc.ServerStream
	recvChan chan *cb.Envelope
	sendChan chan *ab.BroadcastResponse
}

func newMockB() *mockB {
	return &mockB{
		recvChan: make(chan *cb.Envelope),
		sendChan: make(chan *ab.BroadcastResponse),
	}
}

func (m *mockB) Send(br *ab.BroadcastResponse) error {
	m.sendChan <- br
	return nil
}

func (m *mockB) Recv() (*cb.Envelope, error) {
	msg, ok := <-m.recvChan
	if !ok {
		return msg, io.EOF
	}
	return msg, nil
}

type erroneousRecvMockB struct {
	grpc.ServerStream
}

func (m *erroneousRecvMockB) Send(br *ab.BroadcastResponse) error {
	return nil
}

func (m *erroneousRecvMockB) Recv() (*cb.Envelope, error) {
	// The point here is to simulate an error other than EOF.
	// We don't bother to create a new custom error type.
	return nil, io.ErrUnexpectedEOF
}

type erroneousSendMockB struct {
	grpc.ServerStream
	recvVal *cb.Envelope
}

func (m *erroneousSendMockB) Send(br *ab.BroadcastResponse) error {
	// The point here is to simulate an error other than EOF.
	// We don't bother to create a new custom error type.
	return io.ErrUnexpectedEOF
}

func (m *erroneousSendMockB) Recv() (*cb.Envelope, error) {
	return m.recvVal, nil
}

type mockSupportManager struct {
	MsgProcessorIsConfig bool
	MsgProcessorVal      *mockSupport
	MsgProcessorErr      error
}

func (mm *mockSupportManager) BroadcastChannelSupport(msg *cb.Envelope) (*cb.ChannelHeader, bool, ChannelSupport, error) {
	return &cb.ChannelHeader{}, mm.MsgProcessorIsConfig, mm.MsgProcessorVal, mm.MsgProcessorErr
}

type mockSupport struct {
	ProcessConfigEnv *cb.Envelope
	ProcessConfigSeq uint64
	ProcessErr       error
	rejectEnqueue    bool
}

// Order sends a message for ordering
func (ms *mockSupport) Order(env *cb.Envelope, configSeq uint64) error {
	if ms.rejectEnqueue {
		return fmt.Errorf("Reject")
	}
	return nil
}

// Configure sends a reconfiguration message for ordering
func (ms *mockSupport) Configure(configUpdate *cb.Envelope, config *cb.Envelope, configSeq uint64) error {
	return ms.Order(config, configSeq)
}

func (ms *mockSupport) ClassifyMsg(chdr *cb.ChannelHeader) (msgprocessor.Classification, error) {
	panic("UNIMPLMENTED")
}

func (ms *mockSupport) ProcessNormalMsg(msg *cb.Envelope) (uint64, error) {
	return ms.ProcessConfigSeq, ms.ProcessErr
}

func (ms *mockSupport) ProcessConfigUpdateMsg(msg *cb.Envelope) (*cb.Envelope, uint64, error) {
	return ms.ProcessConfigEnv, ms.ProcessConfigSeq, ms.ProcessErr
}

func getMockSupportManager() *mockSupportManager {
	return &mockSupportManager{
		MsgProcessorVal: &mockSupport{},
	}
}

func TestEnqueueFailure(t *testing.T) {
	mm := getMockSupportManager()
	bh := NewHandlerImpl(mm)
	m := newMockB()
	defer close(m.recvChan)
	done := make(chan struct{})
	go func() {
		bh.Handle(m)
		close(done)
	}()

	for i := 0; i < 2; i++ {
		m.recvChan <- nil
		reply := <-m.sendChan
		if reply.Status != cb.Status_SUCCESS {
			t.Fatalf("Should have successfully queued the message")
		}
	}

	mm.MsgProcessorVal.rejectEnqueue = true
	m.recvChan <- nil
	reply := <-m.sendChan
	if reply.Status != cb.Status_SERVICE_UNAVAILABLE {
		t.Fatalf("Should not have successfully queued the message")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Should have terminated the stream")
	}
}

func TestBadChannelId(t *testing.T) {
	mm := getMockSupportManager()
	mm.MsgProcessorVal = &mockSupport{ProcessErr: msgprocessor.ErrChannelDoesNotExist}
	bh := NewHandlerImpl(mm)
	m := newMockB()
	defer close(m.recvChan)
	done := make(chan struct{})
	go func() {
		bh.Handle(m)
		close(done)
	}()

	m.recvChan <- nil
	reply := <-m.sendChan
	if reply.Status != cb.Status_NOT_FOUND {
		t.Fatalf("Should have rejected message to a chain which does not exist")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Should have terminated the stream")
	}
}

func TestGoodConfigUpdate(t *testing.T) {
	mm := getMockSupportManager()
	mm.MsgProcessorIsConfig = true
	bh := NewHandlerImpl(mm)
	m := newMockB()
	defer close(m.recvChan)
	go bh.Handle(m)

	m.recvChan <- nil
	reply := <-m.sendChan
	assert.Equal(t, cb.Status_SUCCESS, reply.Status, "Should have allowed a good CONFIG_UPDATE")
}

func TestBadConfigUpdate(t *testing.T) {
	mm := getMockSupportManager()
	mm.MsgProcessorIsConfig = true
	mm.MsgProcessorVal.ProcessErr = fmt.Errorf("Error")
	bh := NewHandlerImpl(mm)
	m := newMockB()
	defer close(m.recvChan)
	go bh.Handle(m)

	m.recvChan <- nil
	reply := <-m.sendChan
	assert.NotEqual(t, cb.Status_SUCCESS, reply.Status, "Should have rejected CONFIG_UPDATE")
}

func TestGracefulShutdown(t *testing.T) {
	bh := NewHandlerImpl(nil)
	m := newMockB()
	close(m.recvChan)
	assert.NoError(t, bh.Handle(m), "Should exit normally upon EOF")
}

func TestRejected(t *testing.T) {
	mm := &mockSupportManager{
		MsgProcessorVal: &mockSupport{ProcessErr: fmt.Errorf("Reject")},
	}
	bh := NewHandlerImpl(mm)
	m := newMockB()
	defer close(m.recvChan)
	go bh.Handle(m)

	m.recvChan <- nil
	reply := <-m.sendChan
	assert.Equal(t, cb.Status_BAD_REQUEST, reply.Status, "Should have rejected CONFIG_UPDATE")
	assert.Equal(t, mm.MsgProcessorVal.ProcessErr.Error(), reply.Info, "Should have rejected CONFIG_UPDATE")
}

func TestBadStreamRecv(t *testing.T) {
	bh := NewHandlerImpl(nil)
	assert.Error(t, bh.Handle(&erroneousRecvMockB{}), "Should catch unexpected stream error")
}

func TestBadStreamSend(t *testing.T) {
	mm := getMockSupportManager()
	bh := NewHandlerImpl(mm)
	m := &erroneousSendMockB{recvVal: nil}
	assert.Error(t, bh.Handle(m), "Should catch unexpected stream error")
}