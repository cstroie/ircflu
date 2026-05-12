package triviaCmd

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muesli/ircflu/app"
	"github.com/muesli/ircflu/commands"
	"github.com/muesli/ircflu/msgsystem"
)

type TriviaQuestion struct {
	Category string
	Question string
	Answer   string
	Regexp   string
}

type TriviaCommand struct {
	messagesIn  chan msgsystem.Message
	messagesOut chan msgsystem.Message

	questions      []TriviaQuestion
	current        *TriviaQuestion
	currentChannel string // channel where the active question was asked
	active         bool
	stopped        bool
	scores         map[string]int
	timer          *time.Timer
	mu             sync.Mutex

	triviaFile string
	timeout    int
}

func (cmd *TriviaCommand) Name() string {
	return "trivia"
}

func (cmd *TriviaCommand) Run(channelIn, channelOut chan msgsystem.Message) {
	cmd.messagesIn = channelIn
	cmd.messagesOut = channelOut
	cmd.scores = make(map[string]int)

	var err error
	cmd.questions, err = loadQuestions(cmd.triviaFile)
	if err != nil {
		fmt.Println("Trivia: failed to load questions:", err)
		return
	}
	fmt.Printf("Trivia: loaded %d questions\n", len(cmd.questions))
}

func (cmd *TriviaCommand) Parse(msg msgsystem.Message) bool {
	text := strings.TrimSpace(msg.Msg)
	channel := ""
	if len(msg.To) > 0 {
		channel = msg.To[0]
	}

	switch text {
	case "!trivia stop", "!trivia off":
		cmd.mu.Lock()
		cmd.stopped = true
		if cmd.timer != nil {
			cmd.timer.Stop()
			cmd.timer = nil
		}
		cmd.active = false
		cmd.mu.Unlock()
		cmd.send(channel, "Trivia stopped. Type !trivia to start again.")
		return true

	case "!trivia":
		cmd.mu.Lock()
		cmd.stopped = false
		if cmd.active && cmd.timer != nil {
			cmd.timer.Stop()
			cmd.timer = nil
			cmd.active = false
			current := cmd.current
			ch := cmd.currentChannel
			cmd.mu.Unlock()
			cmd.send(ch, fmt.Sprintf("Skipping! The answer was: %s", current.Answer))
			go func() {
				time.Sleep(time.Duration(cmd.timeout) * time.Second)
				cmd.askQuestion(channel)
			}()
		} else {
			cmd.mu.Unlock()
			go cmd.askQuestion(channel)
		}
		return true

	case "!skip":
		cmd.mu.Lock()
		if !cmd.active {
			cmd.mu.Unlock()
			return true
		}
		if cmd.timer != nil {
			cmd.timer.Stop()
			cmd.timer = nil
		}
		cmd.active = false
		current := cmd.current
		ch := cmd.currentChannel
		cmd.mu.Unlock()
		cmd.send(ch, fmt.Sprintf("Skipping! The answer was: %s", current.Answer))
		go func() {
			time.Sleep(time.Duration(cmd.timeout) * time.Second)
			cmd.askQuestion(ch)
		}()
		return true

	case "!score":
		cmd.mu.Lock()
		scores := make(map[string]int, len(cmd.scores))
		for k, v := range cmd.scores {
			scores[k] = v
		}
		cmd.mu.Unlock()
		cmd.send(channel, cmd.formatScores(scores))
		return true
	}

	// Check if this is an answer attempt
	cmd.mu.Lock()
	if !cmd.active || cmd.current == nil {
		cmd.mu.Unlock()
		return false
	}
	if channel != cmd.currentChannel {
		cmd.mu.Unlock()
		return false
	}
	current := cmd.current
	ch := cmd.currentChannel
	cmd.mu.Unlock()

	if checkAnswer(text, *current) {
		nick := msg.Source
		if idx := strings.Index(nick, "!"); idx > 0 {
			nick = nick[:idx]
		}

		cmd.mu.Lock()
		if cmd.timer != nil {
			cmd.timer.Stop()
			cmd.timer = nil
		}
		cmd.active = false
		cmd.scores[nick]++
		cmd.mu.Unlock()

		cmd.send(ch, fmt.Sprintf("Correct! %s wins! The answer was: %s", nick, current.Answer))
		go func() {
			time.Sleep(time.Duration(cmd.timeout) * time.Second)
			cmd.askQuestion(ch)
		}()
		return true
	}

	return false
}

func (cmd *TriviaCommand) askQuestion(channel string) {
	if len(cmd.questions) == 0 {
		fmt.Println("Trivia: no questions loaded")
		return
	}

	cmd.mu.Lock()
	if cmd.active || cmd.stopped {
		cmd.mu.Unlock()
		return
	}
	q := cmd.questions[rand.Intn(len(cmd.questions))]
	cmd.current = &q
	cmd.currentChannel = channel
	cmd.active = true

	timeout := time.Duration(cmd.timeout) * time.Second
	cmd.timer = time.AfterFunc(timeout, func() {
		cmd.mu.Lock()
		cmd.active = false
		cmd.timer = nil
		answer := cmd.current.Answer
		ch := cmd.currentChannel
		cmd.mu.Unlock()

		cmd.send(ch, fmt.Sprintf("Time's up! The answer was: %s", answer))
		time.Sleep(time.Duration(cmd.timeout) * time.Second)
		cmd.askQuestion(ch)
	})
	cmd.mu.Unlock()

	cmd.send(channel, fmt.Sprintf("[%s] %s", q.Category, q.Question))
}

func (cmd *TriviaCommand) send(to, msg string) {
	m := msgsystem.Message{Msg: msg}
	if to != "" {
		m.To = []string{to}
	}
	cmd.messagesOut <- m
}

func (cmd *TriviaCommand) formatScores(scores map[string]int) string {
	if len(scores) == 0 {
		return "No scores yet!"
	}
	type kv struct {
		nick  string
		score int
	}
	var sorted []kv
	for k, v := range scores {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].score > sorted[j].score })

	parts := make([]string, 0, len(sorted))
	for i, kv := range sorted {
		parts = append(parts, fmt.Sprintf("%d. %s (%d)", i+1, kv.nick, kv.score))
	}
	return "Scores: " + strings.Join(parts, " | ")
}

// normalize collapses unicode punctuation/dashes and extra whitespace so that
// "R2‑D2" matches "R2D2", "it's" matches "its", etc.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '\t':
			b.WriteRune(' ')
		// treat all other chars (dashes, punctuation, unicode) as spaces
		default:
			b.WriteRune(' ')
		}
	}
	// collapse runs of spaces
	return strings.Join(strings.Fields(b.String()), " ")
}

func checkAnswer(input string, q TriviaQuestion) bool {
	if q.Regexp != "" {
		re, err := regexp.Compile("(?i)" + q.Regexp)
		if err == nil && re.MatchString(input) {
			return true
		}
	}

	answer := q.Answer
	re := regexp.MustCompile(`#([^#]+)#`)
	matches := re.FindAllStringSubmatch(answer, -1)
	if len(matches) > 0 {
		normInput := normalize(input)
		for _, m := range matches {
			if !strings.Contains(normInput, normalize(m[1])) {
				return false
			}
		}
		return true
	}

	// Plain answer: accept if the answer appears anywhere in the input
	// (handles full-sentence responses like "Mount Whitney is the highest...")
	normInput := normalize(input)
	normAnswer := normalize(answer)
	return strings.Contains(normInput, normAnswer)
}

func loadQuestions(path string) ([]TriviaQuestion, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var questions []TriviaQuestion
	var current TriviaQuestion
	inBlock := false

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}

		if line == "" {
			if inBlock && current.Question != "" && current.Answer != "" {
				questions = append(questions, current)
			}
			current = TriviaQuestion{}
			inBlock = false
			continue
		}

		if after, ok := strings.CutPrefix(line, "Category:"); ok {
			current.Category = strings.TrimSpace(after)
			inBlock = true
		} else if after, ok := strings.CutPrefix(line, "Question:"); ok {
			q := strings.TrimSpace(after)
			// replace fill-in-the-blank dots with underscores
			q = strings.ReplaceAll(q, "·", "_")
			current.Question = q
			inBlock = true
		} else if after, ok := strings.CutPrefix(line, "Answer:"); ok {
			current.Answer = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "Regexp:"); ok {
			current.Regexp = strings.TrimSpace(after)
		}
		// silently skip Author, Level, Comment, Score, Tip, TipCycle
	}
	// flush last block
	if inBlock && current.Question != "" && current.Answer != "" {
		questions = append(questions, current)
	}

	return questions, scanner.Err()
}

func init() {
	cmd := TriviaCommand{}

	app.AddFlags([]app.CliFlag{
		{V: &cmd.triviaFile, Name: "triviafile", Value: "trivia.txt", Desc: "Path to trivia questions file"},
		{V: &cmd.timeout, Name: "triviatimeout", Value: 30, Desc: "Seconds to wait for an answer before revealing it"},
	})

	commands.RegisterCommand(&cmd)
}
