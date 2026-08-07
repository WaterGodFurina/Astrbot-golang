package plugin

import (
        "context"
        "testing"
)

// TestHandlerRegistry_Commands verifies command registration.
func TestHandlerRegistry_Commands(t *testing.T) {
        reg := NewHandlerRegistry()

        reg.RegisterCommand(CommandHandler{
                Name:        "ping",
                Description: "Ping pong",
                Handler:     func(ctx context.Context, args []string) (string, error) { return "pong", nil },
        })

        cmds := reg.Commands()
        if len(cmds) != 1 {
                t.Fatalf("expected 1 command, got %d", len(cmds))
        }
        if cmds[0].Name != "ping" {
                t.Errorf("expected 'ping', got '%s'", cmds[0].Name)
        }

        reg.RegisterCommand(CommandHandler{
                Name:    "pong",
                Aliases: []string{"ping2"},
                Handler: func(ctx context.Context, args []string) (string, error) { return "ping", nil },
        })

        cmds = reg.Commands()
        if len(cmds) != 2 {
                t.Errorf("expected 2 commands, got %d", len(cmds))
        }
}

// TestHandlerRegistry_Hooks verifies hook registration.
func TestHandlerRegistry_Hooks(t *testing.T) {
        reg := NewHandlerRegistry()

        reg.RegisterHook(HookHandler{
                Name:  "test_hook",
                Event: "startup",
                Handler: func(ctx context.Context) error { return nil },
        })

        hooks := reg.Hooks()
        if len(hooks) != 1 {
                t.Fatalf("expected 1 hook, got %d", len(hooks))
        }
        if hooks[0].Event != "startup" {
                t.Errorf("expected event 'startup', got '%s'", hooks[0].Event)
        }
}

// TestManager_Empty ensures a new manager has no plugins.
func TestManager_Empty(t *testing.T) {
        m := NewManager(&Context{})
        if len(m.List()) != 0 {
                t.Error("new manager should have no plugins")
        }
        if len(m.AllCommands()) != 0 {
                t.Error("new manager should have no commands")
        }
}

// TestCommandHandler_Aliases verifies alias storage.
func TestCommandHandler_Aliases(t *testing.T) {
        reg := NewHandlerRegistry()

        reg.RegisterCommand(CommandHandler{
                Name:    "search",
                Aliases: []string{"s", "find", "query"},
                Handler: func(ctx context.Context, args []string) (string, error) { return "", nil },
        })

        cmds := reg.Commands()
        if len(cmds) != 1 {
                t.Fatalf("expected 1 command, got %d", len(cmds))
        }
        if len(cmds[0].Aliases) != 3 {
                t.Errorf("expected 3 aliases, got %d", len(cmds[0].Aliases))
        }
}
