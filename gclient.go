package simplegemini

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"cloud.google.com/go/vertexai/genai"
	"github.com/xyproto/env/v2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
)

type GeminiClient struct {
	Client              *genai.Client
	Functions           map[string]reflect.Value // For custom functions that the LLM can call
	ModelName           string
	MultiModalModelName string
	ProjectLocation     string
	ProjectID           string
	Tools               []*genai.Tool
	Parts               []genai.Part
	Timeout             time.Duration
	Temperature         float32
	Trim                bool
	Verbose             bool
}

const (
	defaultModelName             = "gemini-1.5-flash" // "gemini-1.5-pro" is also a possibility
	defaultMultiModalModelName   = "gemini-1.0-pro-vision"
	defaultProjectLocation       = "us-central1"
	defaultProjectID             = ""
	defaultTimeout               = 3 * time.Minute // pretty long, on purpose
	defaultTemperature           = 0.0
	defaultMultiModalTemperature = 0.4
	defaultTrim                  = true
	defaultVerbose               = false
)

var (
	ErrGoogleCloudProjectID = errors.New("please set GCP_PROJECT or PROJECT_ID to your Google Cloud project ID")
)

func NewCustom(modelName, multiModalModelName, projectLocation, projectID string, temperature float32, timeout time.Duration) (*GeminiClient, error) {
	gc := &GeminiClient{
		ModelName:           env.Str("MODEL_NAME", modelName),
		MultiModalModelName: env.Str("MULTI_MODAL_MODEL_NAME", multiModalModelName),
		ProjectLocation:     env.StrAlt("GCP_LOCATION", "PROJECT_LOCATION", projectLocation),
		ProjectID:           env.StrAlt("GCP_PROJECT", "PROJECT_ID", projectID),
		Timeout:             timeout,
		Temperature:         temperature,
		Tools:               []*genai.Tool{},
		Functions:           make(map[string]reflect.Value),
		Trim:                defaultTrim,
		Verbose:             defaultVerbose,
		Parts:               make([]genai.Part, 0),
	}
	if gc.ProjectID == "" {
		return nil, ErrGoogleCloudProjectID
	}
	ctx := context.Background()
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("failed to obtain default credentials: %v", err)
	}
	genaiClient, err := genai.NewClient(ctx, gc.ProjectID, gc.ProjectLocation, option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("failed to create genai client: %v", err)
	}
	gc.Client = genaiClient
	return gc, nil
}

func New(modelName string, temperature float32) (*GeminiClient, error) {
	// The Google Cloud Project ID is fetched from $GCP_PROJECT or $PROJECT_ID instead.
	return NewCustom(modelName, defaultMultiModalModelName, defaultProjectLocation, defaultProjectID, temperature, defaultTimeout)
}

func MustNew() *GeminiClient {
	gc, err := New(defaultModelName, defaultTemperature)
	if err != nil {
		panic(err)
	}
	return gc
}

func NewText(modelName, projectLocation, projectID string, temperature float32) (*GeminiClient, error) {
	return NewCustom(modelName, defaultMultiModalModelName, projectLocation, projectID, temperature, defaultTimeout)
}

func MustNewText(modelName string, temperature float32) *GeminiClient {
	// The Google Cloud Project ID is fetched from $GCP_PROJECT or $PROJECT_ID instead.
	gc, err := NewText(modelName, defaultProjectLocation, defaultProjectID, temperature)
	if err != nil {
		panic(err)
	}
	return gc
}

func NewWithTimeout(modelName string, temperature float32, timeout time.Duration) (*GeminiClient, error) {
	// The Google Cloud Project ID is fetched from $GCP_PROJECT or $PROJECT_ID instead.
	return NewCustom(modelName, defaultMultiModalModelName, defaultProjectLocation, defaultProjectID, temperature, timeout)
}

func MustNewWithTimeout(modelName string, temperature float32, timeout time.Duration) *GeminiClient {
	// The Google Cloud Project ID is fetched from $GCP_PROJECT or $PROJECT_ID instead.
	gc, err := NewCustom(modelName, defaultMultiModalModelName, defaultProjectLocation, defaultProjectID, temperature, timeout)
	if err != nil {
		panic(err)
	}
	return gc
}

// MultiQuery processes a prompt with optional base64-encoded data and MIME type for the data.
func (gc *GeminiClient) MultiQuery(prompt string, base64Data, dataMimeType *string, temperature *float32) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", ErrEmptyPrompt
	}

	gc.ClearParts()
	gc.AddText(prompt)

	// If base64Data and dataMimeType are provided, decode the data and add it to the multimodal instance.
	if base64Data != nil && dataMimeType != nil {
		data, err := base64.StdEncoding.DecodeString(*base64Data)
		if err != nil {
			return "", fmt.Errorf("failed to decode base64 data: %v", err)
		}
		gc.AddData(*dataMimeType, data)
	}

	ctx, cancel := context.WithTimeout(context.Background(), gc.Timeout)
	defer cancel()

	// Set up the model with tools and start a chat session.
	model := gc.Client.GenerativeModel(gc.ModelName)
	if temperature != nil {
		model.SetTemperature(*temperature)
	}
	model.Tools = gc.Tools
	session := model.StartChat()

	// Submit the multimodal query and process the result.
	res, err := session.SendMessage(ctx, genai.Text(prompt))
	if err != nil {
		return "", fmt.Errorf("failed to send message: %v", err)
	}

	// Handle function calls if present.
	for _, candidate := range res.Candidates {
		for _, part := range candidate.Content.Parts {
			if funcall, ok := part.(genai.FunctionCall); ok {
				// Invoke the user-defined function using reflection.
				responseData, err := gc.invokeFunction(funcall.Name, funcall.Args)
				if err != nil {
					return "", fmt.Errorf("failed to handle function call: %v", err)
				}

				// Send the function response back to the model.
				res, err = session.SendMessage(ctx, genai.FunctionResponse{
					Name:     funcall.Name,
					Response: responseData,
				})
				if err != nil {
					return "", fmt.Errorf("failed to send function response: %v", err)
				}

				var finalResult strings.Builder
				// Process the final response from the LLM.
				for _, part := range res.Candidates[0].Content.Parts {
					if textPart, ok := part.(genai.Text); ok {
						finalResult.WriteString(string(textPart))
						finalResult.WriteString("\n")
					}
				}
				return strings.TrimSpace(finalResult.String()), nil
			}
		}
	}

	// Handle the usual case where no function call is made.
	result, err := gc.SubmitToClient(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to process response: %v", err)
	}

	return strings.TrimSpace(result), nil
}

func (gc *GeminiClient) Query(prompt string) (string, error) {
	return gc.MultiQuery(prompt, nil, nil, nil)
}

// QueryWithCallbacks allows querying with a prompt and processing function calls via a callback handler.
func (gc *GeminiClient) QueryWithCallbacks(prompt string, callback FunctionCallHandler) (string, error) {
	return gc.MultiQueryWithCallbacks(prompt, nil, nil, nil, callback)
}

// QueryWithSequentialCallbacks allows querying with a prompt and processing multiple function calls in sequence via a map of callback handlers.
func (gc *GeminiClient) QueryWithSequentialCallbacks(prompt string, callbacks map[string]FunctionCallHandler) (string, error) {
	return gc.MultiQueryWithSequentialCallbacks(prompt, callbacks)
}

func Ask(prompt string, temperature float32) (string, error) {
	gc, err := NewWithTimeout(defaultModelName, temperature, 10*time.Second)
	if err != nil {
		return "", err
	}
	result, err := gc.Query(prompt)
	if err != nil {
		return "", err
	}
	return result, nil
}

func MustAsk(prompt string, temperature float32) string {
	result, err := Ask(prompt, temperature)
	if err != nil {
		panic(err)
	}
	return result
}

// New creates a new MultiModal instance with a specified model name and temperature,
// initializing it with default values for parts, trim, and verbose settings.
func NewMultiModal(modelName string, temperature float32) (*GeminiClient, error) {
	const projectID = "" // The Google Cloud Project ID is fetched from $GCP_PROJECT or $PROJECT_ID instead.
	return NewCustom(modelName, defaultMultiModalModelName, defaultProjectLocation, projectID, temperature, defaultTimeout)
}

func (gc *GeminiClient) SetTimeout(timeout time.Duration) {
	gc.Timeout = timeout
}

// SetVerbose updates the verbose logging flag of the MultiModal instance,
// allowing for more detailed output during operations.
func (gc *GeminiClient) SetVerbose(verbose bool) {
	gc.Verbose = verbose
}

// SetTrim updates the trim flag of the MultiModal instance,
// controlling whether the output is trimmed for whitespace.
func (gc *GeminiClient) SetTrim(trim bool) {
	gc.Trim = trim
}

// CountTextTokensWithClient will count the tokens in the given text.
func (gc *GeminiClient) CountTextTokensWithClient(ctx context.Context, client *genai.Client, text string) (int, error) {
	model := client.GenerativeModel(gc.ModelName)
	resp, err := model.CountTokens(ctx, genai.Text(text))
	if err != nil {
		return 0, err
	}
	return int(resp.TotalTokens), nil
}

// CountTokensWithClient will count the tokens in the current multimodal prompt.
func (gc *GeminiClient) CountTokensWithClient(ctx context.Context) (int, error) {
	model := gc.Client.GenerativeModel(gc.ModelName)
	var sum int
	for _, part := range gc.Parts {
		resp, err := model.CountTokens(ctx, part)
		if err != nil {
			return sum, err
		}
		sum += int(resp.TotalTokens)
	}
	return sum, nil
}

// SubmitToClient sends all added parts to the specified Vertex AI model for processing,
// returning the model's response. It supports temperature configuration and response trimming.
func (gc *GeminiClient) SubmitToClient(ctx context.Context) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic occurred: %v", r)
		}
	}()
	// Configure the model.
	model := gc.Client.GenerativeModel(gc.ModelName)
	model.SetTemperature(gc.Temperature)
	// Pass in the parts and generate a response.
	res, err := model.GenerateContent(ctx, gc.Parts...)
	if err != nil {
		return "", fmt.Errorf("unable to generate contents: %v", err)
	}
	// Examine the response defensively.
	if res == nil || len(res.Candidates) == 0 || res.Candidates[0] == nil ||
		res.Candidates[0].Content == nil || res.Candidates[0].Content.Parts == nil ||
		len(res.Candidates[0].Content.Parts) == 0 {
		return "", errors.New("empty response from model")
	}
	// Return the result as a string.
	result = fmt.Sprintf("%s\n", res.Candidates[0].Content.Parts[0])
	if gc.Trim {
		return strings.TrimSpace(result), nil
	}
	return result, nil
}

// Submit sends all added parts to the specified Vertex AI model for processing,
// returning the model's response. It supports temperature configuration and response trimming.
// This function creates a temporary client and is not meant to be used within Google Cloud (use SubmitToClient instead).
func (gc *GeminiClient) Submit() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gc.Timeout)
	defer cancel()
	return gc.SubmitToClient(ctx)
}

// CountTokens creates a new client and then counts the tokens in the current multimodal prompt.
func (gc *GeminiClient) CountTokens() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gc.Timeout)
	defer cancel()
	return gc.CountTokensWithClient(ctx)
}

// CountTextTokens tries to count the number of tokens in the given prompt, using the Vertex AI API.
func (gc *GeminiClient) CountTextTokens(prompt string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gc.Timeout)
	defer cancel()
	return gc.CountTextTokensWithClient(ctx, gc.Client, prompt)
}

// Clear clears the prompt parts, tools, and functions registered with the client.
func (gc *GeminiClient) Clear() {
	gc.ClearParts() // Not really needed, since Query also calls this.
	gc.ClearToolsAndFunctions()
}
