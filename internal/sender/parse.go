package sender

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ---- shared shapes ----------------------------------------------------------

type oaUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	InputTokens         int64 `json:"input_tokens"`  // Responses API
	OutputTokens        int64 `json:"output_tokens"` // Responses API
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u *oaUsage) apply(r *Result) {
	if u == nil {
		return
	}
	r.InputTokens = u.PromptTokens + u.InputTokens
	r.OutputTokens = u.CompletionTokens + u.OutputTokens
	switch {
	case u.PromptTokensDetails != nil:
		r.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	case u.InputTokensDetails != nil:
		r.CacheReadTokens = u.InputTokensDetails.CachedTokens
	}
}

type anUsage struct {
	InputTokens          *int64 `json:"input_tokens"`
	OutputTokens         *int64 `json:"output_tokens"`
	CacheReadInputTokens *int64 `json:"cache_read_input_tokens"`
}

func (u *anUsage) apply(r *Result) {
	if u == nil {
		return
	}
	if u.InputTokens != nil {
		r.InputTokens = *u.InputTokens
	}
	if u.OutputTokens != nil {
		r.OutputTokens = *u.OutputTokens
	}
	if u.CacheReadInputTokens != nil {
		r.CacheReadTokens = *u.CacheReadInputTokens
	}
}

type gmUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
}

func (u *gmUsage) apply(r *Result) {
	if u == nil {
		return
	}
	r.InputTokens = u.PromptTokenCount
	// Thinking tokens are billed as output, as completion_tokens is for OpenAI.
	r.OutputTokens = u.CandidatesTokenCount + u.ThoughtsTokenCount
	r.CacheReadTokens = u.CachedContentTokenCount
}

type gmResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text    string `json:"text"`
				Thought bool   `json:"thought"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata *gmUsage        `json:"usageMetadata"`
	Error         json.RawMessage `json:"error"`
}

// text returns visible and thinking text of the first candidate.
func (g *gmResponse) text() (visible, thinking string) {
	if len(g.Candidates) == 0 {
		return "", ""
	}
	var v, th strings.Builder
	for _, p := range g.Candidates[0].Content.Parts {
		if p.Thought {
			th.WriteString(p.Text)
		} else {
			v.WriteString(p.Text)
		}
	}
	return v.String(), th.String()
}

// contentString accepts a string or an array of {type, text} parts.
func contentString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// ---- non-streaming bodies ---------------------------------------------------

func parseBody(protocol string, body []byte, r *Result) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return errors.New("empty response body")
	}
	switch protocol {
	case "openai", "ark":
		return parseOpenAIBody(trimmed, r)
	case "anthropic":
		return parseAnthropicBody(trimmed, r)
	case "gemini":
		return parseGeminiBody(trimmed, r)
	}
	return fmt.Errorf("unsupported protocol %q", protocol)
}

func parseOpenAIBody(body []byte, r *Result) error {
	var v struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Text string `json:"text"` // legacy completions
		} `json:"choices"`
		Output []struct { // Responses API
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *oaUsage        `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("unparseable response body: %v", err)
	}
	var b strings.Builder
	if len(v.Choices) > 0 {
		b.WriteString(contentString(v.Choices[0].Message.Content))
		b.WriteString(v.Choices[0].Text)
	}
	for _, o := range v.Output {
		for _, c := range o.Content {
			if c.Type == "output_text" || c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
	}
	r.OutputText = b.String()
	v.Usage.apply(r)
	if msg := apiErrorText(v.Error); msg != "" {
		appendErr(r, "endpoint error: "+msg)
	}
	return nil
}

func parseAnthropicBody(body []byte, r *Result) error {
	var v struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage *anUsage        `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("unparseable response body: %v", err)
	}
	var b strings.Builder
	for _, c := range v.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	r.OutputText = b.String()
	v.Usage.apply(r)
	if msg := apiErrorText(v.Error); msg != "" {
		appendErr(r, "endpoint error: "+msg)
	}
	return nil
}

func parseGeminiBody(body []byte, r *Result) error {
	var list []gmResponse
	if body[0] == '[' {
		// streamGenerateContent without alt=sse returns a JSON array.
		if err := json.Unmarshal(body, &list); err != nil {
			return fmt.Errorf("unparseable response body: %v", err)
		}
	} else {
		var one gmResponse
		if err := json.Unmarshal(body, &one); err != nil {
			return fmt.Errorf("unparseable response body: %v", err)
		}
		list = []gmResponse{one}
	}
	var b strings.Builder
	for i := range list {
		vis, _ := list[i].text()
		b.WriteString(vis)
		if list[i].UsageMetadata != nil {
			list[i].UsageMetadata.apply(r)
		}
		if msg := apiErrorText(list[i].Error); msg != "" {
			appendErr(r, "endpoint error: "+msg)
		}
	}
	r.OutputText = b.String()
	return nil
}

// ---- server-sent event streams ----------------------------------------------

type streamState struct {
	start  time.Time
	r      *Result
	text   strings.Builder
	events int
	bad    int
}

// delta records streamed output. Thinking text counts for time to first
// token but is not part of output_text.
func (st *streamState) delta(visible, thinking string) {
	if (visible != "" || thinking != "") && st.r.TTFTMS == 0 {
		st.r.TTFTMS = sinceMS(st.start)
	}
	st.text.WriteString(visible)
}

func parseStream(protocol string, body io.Reader, start time.Time, r *Result) error {
	st := &streamState{start: start, r: r}
	var handle func(event string, data []byte) error
	switch protocol {
	case "openai", "ark":
		handle = st.openAIEvent
	case "anthropic":
		handle = st.anthropicEvent
	case "gemini":
		handle = st.geminiEvent
	default:
		return fmt.Errorf("unsupported protocol %q", protocol)
	}

	br := bufio.NewReaderSize(body, 64<<10)
	var event string
	var data bytes.Buffer
	dispatch := func() {
		if data.Len() == 0 {
			event = ""
			return
		}
		d := bytes.TrimSpace(data.Bytes())
		if len(d) > 0 && !bytes.Equal(d, []byte("[DONE]")) {
			st.events++
			if handle(event, d) != nil {
				st.bad++
			}
		}
		event = ""
		data.Reset()
	}
	var readErr error
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if err == nil {
				dispatch()
			}
		} else if line[0] != ':' {
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(field) {
			case "event":
				event = string(value)
			case "data":
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(value)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	dispatch()
	r.OutputText = st.text.String()
	if st.events > 0 && st.bad == st.events {
		appendErr(r, "no parseable stream events")
	}
	return readErr
}

func (st *streamState) openAIEvent(_ string, data []byte) error {
	var v struct {
		Type    string `json:"type"` // Responses API events
		Choices []struct {
			Delta struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				Reasoning        string          `json:"reasoning"`
			} `json:"delta"`
			Text string `json:"text"`
		} `json:"choices"`
		Delta    json.RawMessage `json:"delta"`
		Usage    *oaUsage        `json:"usage"`
		Response *struct {
			Usage *oaUsage `json:"usage"`
		} `json:"response"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	for i, c := range v.Choices {
		if i > 0 {
			break
		}
		st.delta(contentString(c.Delta.Content)+c.Text, c.Delta.ReasoningContent+c.Delta.Reasoning)
	}
	switch v.Type {
	case "response.output_text.delta":
		var s string
		_ = json.Unmarshal(v.Delta, &s)
		st.delta(s, "")
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		var s string
		_ = json.Unmarshal(v.Delta, &s)
		st.delta("", s)
	case "response.completed":
		if v.Response != nil {
			v.Response.Usage.apply(st.r)
		}
	}
	if v.Usage != nil {
		v.Usage.apply(st.r)
	}
	if msg := apiErrorText(v.Error); msg != "" {
		appendErr(st.r, "endpoint error: "+msg)
	}
	return nil
}

func (st *streamState) anthropicEvent(_ string, data []byte) error {
	var v struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *anUsage `json:"usage"`
		} `json:"message"`
		Delta *struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
		Usage *anUsage        `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch v.Type {
	case "message_start":
		if v.Message != nil {
			v.Message.Usage.apply(st.r)
		}
	case "content_block_delta":
		if v.Delta != nil {
			switch v.Delta.Type {
			case "text_delta":
				st.delta(v.Delta.Text, "")
			case "thinking_delta":
				st.delta("", v.Delta.Thinking)
			}
		}
	case "message_delta":
		v.Usage.apply(st.r)
	case "error":
		if msg := apiErrorText(v.Error); msg != "" {
			appendErr(st.r, "endpoint error: "+msg)
		}
	}
	return nil
}

func (st *streamState) geminiEvent(_ string, data []byte) error {
	var v gmResponse
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	st.delta(v.text())
	if v.UsageMetadata != nil {
		v.UsageMetadata.apply(st.r)
	}
	if msg := apiErrorText(v.Error); msg != "" {
		appendErr(st.r, "endpoint error: "+msg)
	}
	return nil
}
