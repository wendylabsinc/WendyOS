package litebuild

import (
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// TUIPrompter asks the questions in the terminal with the CLI prompts.
type TUIPrompter struct {
	In  io.Reader
	Out io.Writer
}

func (p *TUIPrompter) options() []tea.ProgramOption {
	return []tea.ProgramOption{tea.WithInput(p.In), tea.WithOutput(p.Out)}
}

func (p *TUIPrompter) help(q Question) {
	if q.Attr.Display.Help != "" {
		fmt.Fprintln(p.Out, tui.Dim(q.Attr.Display.Help))
	}
}

func (p *TUIPrompter) answered(q Question, value any) {
	fmt.Fprintln(p.Out, tui.SuccessMessage(fmt.Sprintf("%s: %s", q.Attr.Title(), q.Attr.Label(value))))
}

// Select shows a picker with the label of each option.
func (p *TUIPrompter) Select(q Question) (int, error) {
	p.help(q)
	items := make([]tui.PickerItem, len(q.Options))
	defaultKey := ""
	for i, v := range q.Options {
		key := q.Attr.Key(v)
		// SortKey keeps the enum order (the picker sorts by name otherwise).
		items[i] = tui.PickerItem{Name: q.Attr.Label(v), DedupKey: key, SortKey: fmt.Sprintf("%04d", i), Value: i}
		if q.Default != nil && v == q.Default {
			defaultKey = key
		}
	}
	picker := tui.NewPickerWithTitle(q.Attr.Title())
	model, _ := picker.Update(tui.PickerAddMsg{Items: items})
	model, _ = model.Update(tui.PickerDoneMsg{})
	picker = model.(tui.PickerModel)
	picker.FocusItem(defaultKey)

	final, err := tui.RunProgram(tea.NewProgram(picker, p.options()...))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", q.Attr.Title(), err)
	}
	pm := final.(tui.PickerModel)
	if pm.Cancelled() {
		return 0, tui.ErrCancelled
	}
	selected := pm.Selected()
	if selected == nil {
		return 0, fmt.Errorf("%s: no selection", q.Attr.Title())
	}
	i := selected.Value.(int)
	p.answered(q, q.Options[i])
	return i, nil
}

// Confirm asks a yes/no question with the Yes/No prompt of the install
// questions (for example "Set up WiFi on first boot?"). The default is Yes
// when display.default is true, else No.
func (p *TUIPrompter) Confirm(q Question) (bool, error) {
	p.help(q)
	question := q.Attr.Title() + "?"
	var answer bool
	var err error
	if q.Default == true {
		answer, err = tui.ConfirmDefaultYes(question, p.options()...)
	} else {
		answer, err = tui.Confirm(question, p.options()...)
	}
	if err != nil {
		return false, err
	}
	p.answered(q, answer)
	return answer, nil
}

// Text asks a value; validate checks it as it is typed.
func (p *TUIPrompter) Text(q Question, validate func(string) error) (string, error) {
	defaultText := ""
	if q.Default != nil {
		defaultText = q.Attr.Key(q.Default)
	}
	text, err := tui.PromptTextWithDefault(q.Attr.Title(), q.Attr.Display.Help, defaultText, validate, p.options()...)
	if err != nil {
		return "", err
	}
	if v, err := q.Attr.Normalize(text); err == nil {
		p.answered(q, v)
	}
	return text, nil
}

// Info shows a value that needs no question.
func (p *TUIPrompter) Info(attr *Attribute, value any, reason string) {
	fmt.Fprintln(p.Out, tui.SuccessMessage(fmt.Sprintf("%s: %s", attr.Title(), attr.Label(value)))+" "+tui.Dim("("+reason+")"))
}
