package acp

import (
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
)

func TestConvertContentBlocks_TextOnly(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.TextBlock("Hello, world!"),
	}
	result, _ := convertContentBlocks(blocks)
	assert.Equal(t, "Hello, world!", result)
}

func TestConvertContentBlocks_MultipleBlocks(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.TextBlock("First part. "),
		acp.TextBlock("Second part."),
	}
	result, _ := convertContentBlocks(blocks)
	assert.Equal(t, "First part. Second part.", result)
}

func TestConvertContentBlocks_ResourceEmbed(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.TextBlock("Check this file:\n"),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{
				Uri:  "file:///tmp/test.go",
				Text: "package main\n",
			},
		}),
	}
	result, _ := convertContentBlocks(blocks)
	expected := "Check this file:\n--- BEGIN UNTRUSTED FILE CONTENT: /tmp/test.go ---\npackage main\n\n--- END UNTRUSTED FILE CONTENT ---\n"
	assert.Equal(t, expected, result)
}

func TestConvertContentBlocks_ResourceLink(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.ResourceLinkBlock("test.txt", "file:///tmp/test.txt"),
	}
	result, _ := convertContentBlocks(blocks)
	assert.Contains(t, result, "[@file: file:///tmp/test.txt]")
}

func TestConvertContentBlocks_Image(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.TextBlock("What's in this image? "),
		{
			Image: &acp.ContentBlockImage{
				Type:     "image",
				MimeType: "image/png",
				Data:     "iVBORw0KGgoAAAANS", // fake base64
			},
		},
	}
	text, images := convertContentBlocks(blocks)
	assert.Equal(t, "What's in this image? [图片]", text)
	assert.Len(t, images, 1)
	assert.Equal(t, "image/png", images[0].MediaType)
	assert.Equal(t, "iVBORw0KGgoAAAANS", images[0].Data)
}

func TestConvertContentBlocks_MultipleImages(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.TextBlock("Compare these: "),
		{
			Image: &acp.ContentBlockImage{
				Type:     "image",
				MimeType: "image/jpeg",
				Data:     "base64data1",
			},
		},
		{
			Image: &acp.ContentBlockImage{
				Type:     "image",
				MimeType: "image/png",
				Data:     "base64data2",
			},
		},
	}
	text, images := convertContentBlocks(blocks)
	assert.Equal(t, "Compare these: [图片][图片]", text)
	assert.Len(t, images, 2)
	assert.Equal(t, "image/jpeg", images[0].MediaType)
	assert.Equal(t, "image/png", images[1].MediaType)
}

func TestExtractPathFromURI(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"file:///tmp/test.go", "/tmp/test.go"},
		{"file:///home/user/code/main.rs", "/home/user/code/main.rs"},
		{"/tmp/test.go", "/tmp/test.go"},
		{"relative/path.txt", "relative/path.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractPathFromURI(tt.input))
		})
	}
}
