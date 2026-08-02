// Package aggregates holds the write-side models (deciders) of the domain.
package aggregates

import (
	"encoding/json"
	"errors"

	"github.com/JamesTryand/pocketcqrs/decider"
	"github.com/JamesTryand/pocketcqrs/events"
)

// TaskAggregate is the stream/aggregate name for tasks.
const TaskAggregate = "task"

// Task command names.
const (
	CmdCreateTask   = "CreateTask"
	CmdCompleteTask = "CompleteTask"
)

// Task event types.
const (
	TaskCreated   = "TaskCreated"
	TaskCompleted = "TaskCompleted"
)

// TaskState is the folded state of a task stream.
type TaskState struct {
	Exists    bool
	Title     string
	Completed bool
}

// Task returns the decider for the task aggregate.
func Task() *decider.Decider[TaskState] {
	return &decider.Decider[TaskState]{
		InitialState: func() TaskState { return TaskState{} },

		Decide: func(cmd decider.Command, state TaskState) ([]events.NewEvent, error) {
			switch cmd.Name {
			case CmdCreateTask:
				var p struct {
					Title string `json:"title"`
				}
				if err := json.Unmarshal(cmd.Payload, &p); err != nil {
					return nil, err
				}
				if state.Exists {
					return nil, errors.New("task already exists")
				}
				if p.Title == "" {
					return nil, errors.New("title is required")
				}
				data, _ := json.Marshal(map[string]string{"title": p.Title})
				return []events.NewEvent{{Type: TaskCreated, Data: data}}, nil

			case CmdCompleteTask:
				if !state.Exists {
					return nil, errors.New("task does not exist")
				}
				if state.Completed {
					return nil, errors.New("task already completed")
				}
				return []events.NewEvent{{Type: TaskCompleted, Data: json.RawMessage(`{}`)}}, nil

			default:
				return nil, errors.New("unknown command: " + cmd.Name)
			}
		},

		Evolve: func(state TaskState, ev events.Event) (TaskState, error) {
			switch ev.Type {
			case TaskCreated:
				var p struct {
					Title string `json:"title"`
				}
				if err := json.Unmarshal(ev.Data, &p); err != nil {
					return state, err
				}
				state.Exists = true
				state.Title = p.Title
			case TaskCompleted:
				state.Completed = true
			}
			return state, nil
		},
	}
}

// RegisterAll registers every domain decider with the registry.
func RegisterAll(r *decider.Registry) {
	decider.Register(r, TaskAggregate, Task())
	decider.Register(r, OrderAggregate, Order())
}
