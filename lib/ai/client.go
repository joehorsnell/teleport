/*
 * Copyright 2023 Gravitational, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package ai

import (
	"context"

	"github.com/gravitational/trace"
	"github.com/sashabaranov/go-openai"
	"github.com/tiktoken-go/tokenizer/codec"

	"github.com/gravitational/teleport/lib/ai/embedding"
	"github.com/gravitational/teleport/lib/ai/model"
	modeltools "github.com/gravitational/teleport/lib/ai/model/tools"
	"github.com/gravitational/teleport/lib/ai/tokens"
	"github.com/gravitational/teleport/lib/modules"
)

const (
	maxOpenAIEmbeddingsPerRequest = 1000
)

// Client is a client for OpenAI API.
type Client struct {
	svc *openai.Client
}

// NewClient creates a new client for OpenAI API.
func NewClient(authToken string) *Client {
	return &Client{openai.NewClient(authToken)}
}

// NewClientFromConfig creates a new client for OpenAI API from config.
func NewClientFromConfig(config openai.ClientConfig) *Client {
	return &Client{openai.NewClientWithConfig(config)}
}

// NewChat creates a new chat. The username is set in the conversation context,
// so that the AI can use it to personalize the conversation.
// embeddingServiceClient is used to get the embeddings from the Auth Server.
func (client *Client) NewChat(toolContext *modeltools.ToolContext) *Chat {
	tools := []modeltools.Tool{
		&modeltools.CommandExecutionTool{},
		&modeltools.EmbeddingRetrievalTool{},
	}

	// The following tools are only available in the enterprise build. They will fail
	// if included in OSS due to the lack of the required backend APIs.
	if modules.GetModules().BuildType() == modules.BuildEnterprise {
		tools = append(tools, &modeltools.AccessRequestCreateTool{},
			&modeltools.AccessRequestsListTool{},
			&modeltools.AccessRequestListRequestableRolesTool{},
			&modeltools.AccessRequestListRequestableResourcesTool{})
	}

	return &Chat{
		client: client,
		messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: model.PromptCharacter(toolContext.User),
			},
		},
		// Initialize a tokenizer for prompt token accounting.
		// Cl100k is used by GPT-3 and GPT-4.
		tokenizer: codec.NewCl100kBase(),
		agent:     model.NewAgent(toolContext, tools...),
	}
}

func (client *Client) NewCommand(username string) *Chat {
	toolContext := &modeltools.ToolContext{User: username}
	return &Chat{
		client: client,
		messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: model.PromptCharacter(username),
			},
		},
		// Initialize a tokenizer for prompt token accounting.
		// Cl100k is used by GPT-3 and GPT-4.
		tokenizer: codec.NewCl100kBase(),
		agent:     model.NewAgent(toolContext, &modeltools.CommandGenerationTool{}),
	}
}

// Summary creates a short summary for the given input.
func (client *Client) Summary(ctx context.Context, message string) (string, error) {
	resp, err := client.svc.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: openai.GPT4,
			Messages: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: model.PromptSummarizeTitle},
				{Role: openai.ChatMessageRoleUser, Content: message},
			},
		},
	)

	if err != nil {
		return "", trace.Wrap(err)
	}

	return resp.Choices[0].Message.Content, nil
}

// CommandSummary creates a command summary based on the command output.
// The message history is also passed to the model in order to keep context
// and extract relevant information from the output.
func (client *Client) CommandSummary(ctx context.Context, messages []openai.ChatCompletionMessage, output map[string][]byte) (string, *tokens.TokenCount, error) {
	messages = append(messages, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser, Content: model.ConversationCommandResult(output)})

	promptTokens, err := tokens.NewPromptTokenCounter(messages)
	if err != nil {
		return "", nil, trace.Wrap(err)
	}

	resp, err := client.svc.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model:    openai.GPT4,
			Messages: messages,
		},
	)

	if err != nil {
		return "", nil, trace.Wrap(err)
	}

	completion := resp.Choices[0].Message.Content
	completionTokens, err := tokens.NewSynchronousTokenCounter(completion)

	tc := &tokens.TokenCount{Prompt: tokens.TokenCounters{promptTokens}, Completion: tokens.TokenCounters{completionTokens}}
	return completion, tc, trace.Wrap(err)
}

// ClassifyMessage takes a user message, a list of categories, and uses the AI mode as a zero-shot classifier.
func (client *Client) ClassifyMessage(ctx context.Context, message string, classes map[string]string) (string, error) {
	resp, err := client.svc.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: openai.GPT4,
			Messages: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: model.MessageClassificationPrompt(classes)},
				{Role: openai.ChatMessageRoleUser, Content: message},
			},
		},
	)

	if err != nil {
		return "", trace.Wrap(err)
	}

	return resp.Choices[0].Message.Content, nil
}

// ComputeEmbeddings takes a map of nodes and calls openAI to generate
// embeddings for those nodes. ComputeEmbeddings is responsible for
// implementing a retry mechanism if the embedding computation is flaky.
func (client *Client) ComputeEmbeddings(ctx context.Context, input []string) ([]embedding.Vector64, error) {
	var results []embedding.Vector64
	for i := 0; maxOpenAIEmbeddingsPerRequest*i < len(input); i++ {
		result, err := client.computeEmbeddings(ctx, paginateInput(input, i, maxOpenAIEmbeddingsPerRequest))
		if err != nil {
			return nil, trace.Wrap(err)
		}
		for _, vector := range result {
			results = append(results, embedding.Vector32to64(vector))
		}
	}
	return results, nil
}

func paginateInput(input []string, page, pageSize int) []string {
	begin := page * pageSize
	var end int
	if len(input) < (page+1)*pageSize {
		end = len(input)
	} else {
		end = (page + 1) * pageSize
	}
	return input[begin:end]
}

// computeEmbeddings calls the openAI embedding model with the provided input.
// This function should not be called directly, use ComputeEmbeddings instead
// to ensure input is properly batched.
func (client *Client) computeEmbeddings(ctx context.Context, input []string) ([]embedding.Vector32, error) {
	req := openai.EmbeddingRequest{
		Input: input,
		Model: openai.AdaEmbeddingV2,
	}

	// Execute the query
	resp, err := client.svc.CreateEmbeddings(ctx, req)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	result := make([]embedding.Vector32, len(input))
	for i, item := range resp.Data {
		result[i] = item.Embedding
	}
	return result, nil
}
