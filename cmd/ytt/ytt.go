package ytt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/kkdai/youtube/v2"
	"github.com/liushuangls/go-anthropic/v2"
	"github.com/sashabaranov/go-openai"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/thediveo/enumflag/v2"
)

const (
	userPrompt = `You will be given auto-generated captions from a YouTube video. These may be full captions, or a segment of the full transcript if it is too large. Your task is to transform these captions into a clean, readable transcript. Here are the auto-generated captions:

<captions>
%s
</captions>

Follow these steps to create a clean transcript:

1. Correct any spelling errors you encounter. Use your knowledge of common words and context to determine the correct spelling.

2. Add appropriate punctuation throughout the text. This includes commas, periods, question marks, and exclamation points where necessary.

3. Capitalize the first letter of each sentence and proper nouns.

4. Break the text into logical paragraphs. Start a new paragraph when there's a shift in topic or speaker.

5. Remove any unnecessary filler words, repetitions, or false starts.

6. Maintain the original meaning and intent of the transcript. Do not remove any content even if it is unrelated to the main topic.


Once you have completed these steps, provide the clean transcript within <transcript> tags. Ensure that the transcript is well-formatted, easy to read, 
and accurately represents the original content of the video. Do not include any additional text in your response.`
)

var transcriptRegex = regexp.MustCompile(`(?s)<transcript>(.*?)</transcript>`)

func extractTranscript(input string) string {
	match := transcriptRegex.FindStringSubmatch(input)
	if len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

type Model enumflag.Flag

// Enumeration of allowed ColorMode values.
const (
	ModelChatGPT Model = iota
	ModelClaude
)

// Defines the textual representations for the ColorMode values.
var modelMap = map[Model][]string{
	Model(ModelChatGPT): {"chatgpt"},
	Model(ModelClaude):  {"claude"},
}

func callChatGPTAPIWithBackoff(client *openai.Client, text string) (string, error) {

	req := openai.ChatCompletionRequest{
		Model: openai.GPT4o,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: fmt.Sprintf(userPrompt, text),
			},
		},
	}

	backOff := backoff.NewExponentialBackOff()
	backOff.MaxElapsedTime = 10 * time.Minute

	var resp openai.ChatCompletionResponse

	err := backoff.Retry(func() (err error) {
		resp, err = client.CreateChatCompletion(context.Background(), req)
		if err != nil {
			// Check if the error is a 429 (Too Many Requests) error
			var openAIError *openai.APIError
			if errors.As(err, &openAIError) {
				if openAIError.HTTPStatusCode == http.StatusTooManyRequests {
					// This is a 429 error, so we'll retry
					fmt.Printf("%v\n", err)
					fmt.Println("Retrying…")
					return err
				}
			}
			// For any other error, we'll stop retrying
			return backoff.Permanent(err)
		}
		return nil
	}, backOff)

	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices returned from API")
	}

	// TODO: Log this as debug output
	// fmt.Printf("Usage: %+v\n", resp.Usage)
	return resp.Choices[0].Message.Content, nil
}

func callClaudeAPIWithBackoff(client *anthropic.Client, text string) (string, error) {
	req := &anthropic.MessagesRequest{
		Model: anthropic.ModelClaude3Dot5Sonnet20240620,
		Messages: []anthropic.Message{
			anthropic.NewUserTextMessage(fmt.Sprintf(userPrompt, text)),
		},
		MaxTokens: 8192,
	}

	backOff := backoff.NewExponentialBackOff()
	backOff.MaxElapsedTime = 10 * time.Minute

	var resp anthropic.MessagesResponse

	err := backoff.Retry(func() (err error) {
		resp, err = client.CreateMessages(context.Background(), *req)
		if err != nil {
			var anthropicAPIError *anthropic.APIError
			if errors.As(err, &anthropicAPIError) {
				if anthropicAPIError.IsRateLimitErr() || anthropicAPIError.IsOverloadedErr() {
					fmt.Printf("%v\n", err)
					fmt.Println("Retrying…")
					return err
				}
			}
			// For any other error, we'll stop retrying
			return backoff.Permanent(err)
		}
		return nil
	}, backOff)

	if err != nil {
		return "", err
	}

	// TODO: Log this as debug output
	fmt.Printf("Usage: %+v\n", resp.Usage)
	return resp.GetFirstContentText(), nil
}

func chunkTranscript(transcript string, maxWordsPerChunk int) []string {
	// Split the transcript into chunks
	var chunks []string
	scanner := bufio.NewScanner(strings.NewReader(transcript))
	scanner.Split(bufio.ScanWords)

	var chunkBuilder strings.Builder
	wordCount := 0

	for scanner.Scan() {
		word := scanner.Text()
		chunkBuilder.WriteString(word + " ")
		wordCount++
		if wordCount >= maxWordsPerChunk {
			chunks = append(chunks, chunkBuilder.String())
			chunkBuilder.Reset()
			wordCount = 0
		}
	}
	if chunkBuilder.Len() > 0 {
		chunks = append(chunks, chunkBuilder.String())
	}
	return chunks

}

var Command = &cobra.Command{
	Use:   "ytt <youtube_url>",
	Short: "Generate cleaned up transcript from YouTube autogenerated captions using ChatGPT",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, _ := cmd.Flags().GetBool("raw")
		model := cmd.Flags().Lookup("model").Value.String()

		openaiApiKey := viper.GetString("openai_api_key")
		if model == "chatgpt" && openaiApiKey == "" && !raw {
			return errors.New("OpenAI API key not found. Please run 'podscript configure' or set the PODSCRIPT_OPENAI_API_KEY environment variable")
		}

		anthropicApiKey := viper.GetString("anthropic_api_key")
		if model == "claude" && anthropicApiKey == "" && !raw {
			return errors.New("Anthropic API key not found. Please run 'podscript configure' or set the PODSCRIPT_ANTHROPIC_API_KEY environment variable")
		}

		folder, _ := cmd.Flags().GetString("path")
		suffix, _ := cmd.Flags().GetString("suffix")
		if folder != "" {
			fi, err := os.Stat(folder)
			if err != nil || !fi.IsDir() {
				return fmt.Errorf("path not found: %s", folder)
			}
		}
		timestamp := time.Now().Format("2006-01-02-150405")
		var filenameSuffix string
		if suffix == "" {
			filenameSuffix = timestamp
		} else {
			filenameSuffix = fmt.Sprintf("%s_%s", timestamp, suffix)
		}

		// Extract Transcript
		youtubeClient := youtube.Client{}

		video, err := youtubeClient.GetVideo(args[0])
		if err != nil {
			return fmt.Errorf("failed to get video info: %w", err)
		}

		transcript, err := youtubeClient.GetTranscript(video, "en")
		if err != nil {
			return fmt.Errorf("failed to get transcript info: %w", err)
		}

		var transcriptTxt string
		for _, tr := range transcript {
			transcriptTxt += tr.Text + "\n"
		}

		rawTranscriptFilename := path.Join(folder, fmt.Sprintf("raw_transcript_%s.txt", filenameSuffix))
		if err = os.WriteFile(rawTranscriptFilename, []byte(transcriptTxt), 0644); err != nil {
			return fmt.Errorf("failed to write raw transcript: %w", err)
		}
		fmt.Printf("wrote raw autogenerated captions to %s\n", rawTranscriptFilename)

		// Stop if only raw transcript required
		if raw {
			return nil
		}

		var maxWordsPerChunk int
		if model == "chatgpt" {
			maxWordsPerChunk = 3000
		} else if model == "claude" {
			maxWordsPerChunk = 6000
		}
		// Chunk and Send to OpenAI
		chunks := chunkTranscript(transcriptTxt, maxWordsPerChunk)
		// First chunk used as context

		var (
			openAPIClient   *openai.Client
			claudeAPIClient *anthropic.Client
		)

		if model == "chatgpt" {
			openAPIClient = openai.NewClient(openaiApiKey)
		} else {
			claudeAPIClient = anthropic.NewClient(
				anthropicApiKey,
				anthropic.WithBetaVersion(anthropic.BetaMaxTokens35Sonnet20240715))
		}
		caller := func(chunk string) (string, error) {
			if model == "chatgpt" {
				return callChatGPTAPIWithBackoff(openAPIClient, chunk)
			} else if model == "claude" {
				return callClaudeAPIWithBackoff(claudeAPIClient, chunk)
			}
			panic("should never get here")
		}

		var cleanedTranscript strings.Builder
		for i, chunk := range chunks {
			cleanedChunk, err := caller(chunk)
			if err != nil {
				return fmt.Errorf("failed to process chunk: %w", err)
			}
			cleanedChunk = extractTranscript(cleanedChunk)
			cleanedTranscript.WriteString(cleanedChunk)
			fmt.Printf("transcribed part %d/%d…\n", i+1, len(chunks))
		}

		if err != nil {
			return fmt.Errorf("failed to process chunk: %w", err)
		}

		cleanedTranscriptFilename := path.Join(folder, fmt.Sprintf("cleaned_transcript_%s.txt", filenameSuffix))
		if err = os.WriteFile(cleanedTranscriptFilename, []byte(cleanedTranscript.String()), 0644); err != nil {
			return fmt.Errorf("failed to write cleaned transcript: %w", err)
		}
		fmt.Printf("wrote cleaned up transcripts to %s\n", cleanedTranscriptFilename)
		return nil
	},
}

func init() {
	Command.Flags().StringP("path", "p", "", "save raw and cleaned up transcripts to path")
	Command.Flags().StringP("suffix", "s", "", "append suffix to filenames")
	Command.Flags().BoolP("raw", "r", false, "download raw transcript, don't cleanup using LLM")
	Command.Flags().VarP(enumflag.New(new(Model), "model", modelMap, enumflag.EnumCaseInsensitive), "model", "m", "use specified model: can be 'chatgpt' (default if omitted) or 'claude'")
}
