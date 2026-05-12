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

	questions []TriviaQuestion
	current   *TriviaQuestion
	active    bool
	scores    map[string]int
	timer     *time.Timer
	mu        sync.Mutex

	triviaFile    string
	triviaChannel string
	timeout       int
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

	go func() {
		time.Sleep(3 * time.Second)
		cmd.askQuestion()
	}()
}

func (cmd *TriviaCommand) Parse(msg msgsystem.Message) bool {
	text := strings.TrimSpace(msg.Msg)
	channel := ""
	if len(msg.To) > 0 {
		channel = msg.To[0]
	}

	switch text {
	case "!trivia":
		cmd.mu.Lock()
		if cmd.active && cmd.timer != nil {
			cmd.timer.Stop()
			cmd.timer = nil
			cmd.active = false
			current := cmd.current
			cmd.mu.Unlock()
			cmd.send(channel, fmt.Sprintf("Skipping! The answer was: %s", current.Answer))
			go func() {
				time.Sleep(5 * time.Second)
				cmd.askQuestion()
			}()
		} else {
			cmd.mu.Unlock()
			go cmd.askQuestion()
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
		cmd.mu.Unlock()
		cmd.send(channel, fmt.Sprintf("Skipping! The answer was: %s", current.Answer))
		go func() {
			time.Sleep(5 * time.Second)
			cmd.askQuestion()
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
	if channel != cmd.triviaChannel {
		cmd.mu.Unlock()
		return false
	}
	current := cmd.current
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

		cmd.send(cmd.triviaChannel, fmt.Sprintf("Correct! %s wins! The answer was: %s", nick, current.Answer))
		go func() {
			time.Sleep(5 * time.Second)
			cmd.askQuestion()
		}()
		return true
	}

	return false
}

func (cmd *TriviaCommand) askQuestion() {
	if len(cmd.questions) == 0 {
		fmt.Println("Trivia: no questions loaded")
		return
	}

	cmd.mu.Lock()
	if cmd.active {
		cmd.mu.Unlock()
		return
	}
	q := cmd.questions[rand.Intn(len(cmd.questions))]
	cmd.current = &q
	cmd.active = true

	timeout := time.Duration(cmd.timeout) * time.Second
	cmd.timer = time.AfterFunc(timeout, func() {
		cmd.mu.Lock()
		cmd.active = false
		cmd.timer = nil
		answer := cmd.current.Answer
		cmd.mu.Unlock()

		cmd.send(cmd.triviaChannel, fmt.Sprintf("Time's up! The answer was: %s", answer))
		time.Sleep(5 * time.Second)
		cmd.askQuestion()
	})
	cmd.mu.Unlock()

	cmd.send(cmd.triviaChannel, fmt.Sprintf("[%s] %s", q.Category, q.Question))
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

func checkAnswer(input string, q TriviaQuestion) bool {
	if q.Regexp != "" {
		re, err := regexp.Compile("(?i)" + q.Regexp)
		if err == nil && re.MatchString(input) {
			return true
		}
	}

	answer := q.Answer
	// Extract #token# style key words
	re := regexp.MustCompile(`#([^#]+)#`)
	matches := re.FindAllStringSubmatch(answer, -1)
	if len(matches) > 0 {
		lower := strings.ToLower(input)
		for _, m := range matches {
			if !strings.Contains(lower, strings.ToLower(m[1])) {
				return false
			}
		}
		return true
	}

	return strings.EqualFold(strings.TrimSpace(input), strings.TrimSpace(answer))
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
			current.Question = strings.TrimSpace(after)
			inBlock = true
		} else if after, ok := strings.CutPrefix(line, "Answer:"); ok {
			current.Answer = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "Regexp:"); ok {
			current.Regexp = strings.TrimSpace(after)
		}
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
		{V: &cmd.triviaChannel, Name: "triviachannel", Value: "#ircflutest", Desc: "IRC channel to play trivia in"},
		{V: &cmd.timeout, Name: "triviatimeout", Value: 30, Desc: "Seconds to wait for an answer before revealing it"},
	})

	commands.RegisterCommand(&cmd)
}
