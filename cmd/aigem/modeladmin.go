package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
)

// fleetModelAdmin adapts the daemon-owned bot configuration and live process
// facts to internal/chat without making that package depend on internal/bot.
type fleetModelAdmin struct {
	names   map[string]struct{}
	ordered []string
	live    *liveFleet
	service *botModelService
}

func newFleetModelAdmin(names []string, live *liveFleet) *fleetModelAdmin {
	known := make(map[string]struct{}, len(names))
	ordered := append([]string(nil), names...)
	sort.Strings(ordered)
	for _, name := range ordered {
		known[name] = struct{}{}
	}
	return &fleetModelAdmin{
		names: known, ordered: ordered, live: live, service: newBotModelService(),
	}
}

func (a *fleetModelAdmin) Models(ctx context.Context) (chat.BotModels, error) {
	if err := ctx.Err(); err != nil {
		return chat.BotModels{}, err
	}
	out := chat.BotModels{Options: a.options()}
	live := a.liveStatus()
	for _, name := range a.ordered {
		settings, err := a.settings(name, live)
		if err != nil {
			return chat.BotModels{}, err
		}
		out.Bots = append(out.Bots, settings)
	}
	return out, nil
}

func (a *fleetModelAdmin) SetModel(ctx context.Context, name string, requested *string) (chat.BotModelSettings, error) {
	if err := ctx.Err(); err != nil {
		return chat.BotModelSettings{}, err
	}
	if _, ok := a.names[name]; !ok {
		return chat.BotModelSettings{}, fmt.Errorf("%w: %q", chat.ErrNoSuchBot, name)
	}

	configured := ""
	if requested != nil {
		ref := strings.TrimSpace(*requested)
		if ref == "" {
			return chat.BotModelSettings{}, fmt.Errorf("%w: model ref is empty", chat.ErrInvalidModel)
		}
		info, err := resolveBotModel(a.service.registry, ref)
		if err != nil {
			return chat.BotModelSettings{}, fmt.Errorf("%w: %w", chat.ErrInvalidModel, explainPinFailure(ref, err))
		}
		configured = info.Ref()
	}
	cfg, err := bot.Load(name)
	if err != nil {
		return chat.BotModelSettings{}, fmt.Errorf("load bot %q: %w", name, err)
	}
	if _, err := a.service.selection(cfg, configured); err != nil {
		return chat.BotModelSettings{}, fmt.Errorf("%w: %w", chat.ErrInvalidModel, err)
	}
	if _, _, err := a.service.update(name, configured); err != nil {
		return chat.BotModelSettings{}, fmt.Errorf("save model for bot %q: %w", name, err)
	}
	return a.settings(name, a.liveStatus())
}

func (a *fleetModelAdmin) options() []chat.ModelOption {
	models := a.service.registry.Models()
	sort.Slice(models, func(i, j int) bool { return models[i].Ref() < models[j].Ref() })
	options := make([]chat.ModelOption, 0, len(models))
	for _, model := range models {
		option := chat.ModelOption{
			Ref: model.Ref(), Name: model.Name, Provider: model.Provider, Usable: true,
		}
		if _, err := resolveBotModel(a.service.registry, model.Ref()); err != nil {
			option.Usable = false
			option.Reason = err.Error()
		}
		options = append(options, option)
	}
	return options
}

func (a *fleetModelAdmin) liveStatus() map[string]chat.LiveBot {
	if a.live == nil {
		return nil
	}
	return a.live.status()
}

func (a *fleetModelAdmin) settings(name string, live map[string]chat.LiveBot) (chat.BotModelSettings, error) {
	if _, ok := a.names[name]; !ok {
		return chat.BotModelSettings{}, fmt.Errorf("%w: %q", chat.ErrNoSuchBot, name)
	}
	cfg, err := bot.Load(name)
	if err != nil {
		return chat.BotModelSettings{}, fmt.Errorf("load bot %q: %w", name, err)
	}
	selection, err := cfg.ModelSelection()
	if err != nil {
		return chat.BotModelSettings{}, fmt.Errorf("bot %q: %w", name, err)
	}
	settings := chat.BotModelSettings{
		Name: name, Role: cfg.Role, Configured: selection.Configured,
		Selected: selection.Effective, Source: selection.Source,
	}
	if state, ok := live[chat.BotActor(name)]; ok && state.Running {
		settings.Running = state.Model
		settings.RestartRequired = state.Model != selection.Effective
	}
	return settings, nil
}
