package gui

import (
	"fmt"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// todoList is the user's to-do list, pinned to the top of the rail. Solo keeps it as they talk; a todo
// drops off when its threads' work lands, or when the user ticks it.
func (a *App) todoList(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	todos := a.todos.Get()
	folded := ui.Local(ctx, "folded", false)
	chevron := icons.ChevronDown
	if folded.Get() {
		chevron = icons.ChevronRight
	}
	head := ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(28)).Radius(radiusS).
		Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
		Role(access.RoleButton).Label("To do").TabIndex(0).
		OnClick(func() { folded.Update(func(f bool) bool { return !f }) }).
		Child(ui.Text("To do").FontSize(14).Semibold().NoWrap()).
		Child(ui.If(len(todos) > 0, ui.Text(fmt.Sprint(len(todos))).FontSize(12).TextColor(t.TextSubtle))).
		Child(ui.Div().Grow(1)).
		Child(ui.Icon(chevron).Size(14).Color(t.TextSubtle))
	col := ui.Div().Column().Gap(2).PaddingX(space2).PaddingTop(space3).PaddingBottom(space2).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).Child(head)
	if folded.Get() {
		return col
	}
	if len(todos) == 0 {
		return col.Child(ui.Div().PaddingX(space2).PaddingBottom(space1).
			Child(caption(ctx, "Nothing yet. What you ask Solo for shows up here.")))
	}
	// A long list shows its first few in whole rows, and the rest on request, so the threads keep room.
	all := ui.Local(ctx, "all", false)
	shown := todos
	if !all.Get() && len(todos) > todoRows {
		shown = todos[:todoRows]
	}
	list := ui.Div().Column().Gap(1)
	for _, td := range shown {
		list = list.Child(ui.Component(func(ctx *ui.Context) ui.Element { return a.todoRow(ctx, td) }).Key("todo-" + td.ID))
	}
	if len(todos) > todoRows {
		label := fmt.Sprintf("Show %d more", len(todos)-todoRows)
		if all.Get() {
			label = "Show fewer"
		}
		list = list.Child(ui.Div().Row().Align(ui.Center).Height(ui.Px(24)).PaddingLeft(space1 + 2).Radius(radiusS).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(label).TabIndex(0).
			OnClick(func() { all.Update(func(v bool) bool { return !v }) }).
			Child(ui.Text(label).FontSize(12).TextColor(t.TextSubtle)))
	}
	return col.Child(ui.Div().Column().MaxHeight(ui.Px(320)).ScrollY().Child(list))
}

// todoRows is how many todos the rail shows before "Show N more".
const todoRows = 4

// todoRow is one todo: a tick box, its text, and the state of the thread doing it, which a click opens.
func (a *App) todoRow(ctx *ui.Context, td core.Todo) ui.Element {
	t := ctx.Theme()
	hover := ui.Local(ctx, "hover", false)
	var th *core.ThreadView
	for _, id := range td.Threads {
		if v, ok := a.thread(id); ok && v.Status != core.StatusDiscarded {
			th = &v
			if !core.Finished(v.Status) {
				break // the one still working says most
			}
		}
	}
	box := icons.Circle
	boxColor := t.TextSubtle
	if hover.Get() {
		box, boxColor = icons.CircleCheck, t.Success
	}
	tick := ui.Div().Size(ui.Px(20), ui.Px(20)).Center().Shrink(0).Radius(radiusS).Cursor(ui.CursorPointer).
		Role(access.RoleButton).Label("Mark “" + td.Text + "” done").TabIndex(0).Tooltip("Done").
		OnMouseEnter(func() { hover.Set(true) }).OnMouseLeave(func() { hover.Set(false) }).
		OnClick(func() { a.completeTodo(td) }).
		Child(ui.Icon(box).Size(15).Color(boxColor))
	row := ui.Div().Row().Align(ui.Start).Gap(space1 + 2).PaddingX(space1).PaddingY(3).Radius(radiusS).
		Child(tick).
		Child(ui.Div().Grow(1).MinWidth(ui.Px(0)).PaddingTop(2).Child(ui.Text(td.Text).FontSize(13).MaxLines(2)))
	if th != nil {
		id, title := th.ID, th.Title
		row = row.Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Tooltip(title).
			OnClick(func() { a.openThread(id, "conversation") }).
			Child(ui.Div().Height(ui.Px(20)).Row().Align(ui.Center).Child(statusMark(ctx, shownStatus(*th))))
	}
	return row
}

// completeTodo ticks a todo off, with an Undo.
func (a *App) completeTodo(td core.Todo) {
	a.todos.Update(func(ts []core.Todo) []core.Todo { // drop it now; the backend agrees shortly
		out := make([]core.Todo, 0, len(ts))
		for _, x := range ts {
			if x.ID != td.ID {
				out = append(out, x)
			}
		}
		return out
	})
	id, text := td.ID, td.Text
	go func() {
		if err := a.b.CompleteTodo(id); err != nil {
			a.onUI(func() { a.notify(ui.ToastError, "Couldn't complete it", err.Error()) })
			a.onChange(core.Change{Kind: core.ChangeTodos})
			return
		}
		a.onUI(func() {
			if a.undoToast != nil {
				a.undoToast("“"+text+"” done.", func() {
					open := false
					go a.b.UpdateTodo(id, core.TodoChange{Done: &open})
				})
			}
		})
	}()
}
