// Code generated by mockery v2.32.0. DO NOT EDIT.

package client

import (
	context "context"

	queue "github.com/content-services/content-sources-backend/pkg/tasks/queue"
	mock "github.com/stretchr/testify/mock"

	uuid "github.com/google/uuid"
)

// MockTaskClient is an autogenerated mock type for the TaskClient type
type MockTaskClient struct {
	mock.Mock
}

// Enqueue provides a mock function with given fields: task
func (_m *MockTaskClient) Enqueue(task queue.Task) (uuid.UUID, error) {
	ret := _m.Called(task)

	var r0 uuid.UUID
	var r1 error
	if rf, ok := ret.Get(0).(func(queue.Task) (uuid.UUID, error)); ok {
		return rf(task)
	}
	if rf, ok := ret.Get(0).(func(queue.Task) uuid.UUID); ok {
		r0 = rf(task)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(uuid.UUID)
		}
	}

	if rf, ok := ret.Get(1).(func(queue.Task) error); ok {
		r1 = rf(task)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// SendCancelNotification provides a mock function with given fields: ctx, taskId
func (_m *MockTaskClient) SendCancelNotification(ctx context.Context, taskId string) error {
	ret := _m.Called(ctx, taskId)

	var r0 error
	if rf, ok := ret.Get(0).(func(context.Context, string) error); ok {
		r0 = rf(ctx, taskId)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// NewMockTaskClient creates a new instance of MockTaskClient. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewMockTaskClient(t interface {
	mock.TestingT
	Cleanup(func())
}) *MockTaskClient {
	mock := &MockTaskClient{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
