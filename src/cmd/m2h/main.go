package main

import (
	"fmt"
	"io"
	"os"

	"wall-e/chat"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "m2h: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		_, err = stdout.Write([]byte(chat.RenderTelegramMarkdown(string(input))))
		return err
	}

	for i, path := range args {
		input, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if i > 0 {
			if _, err := io.WriteString(stdout, "\n"); err != nil {
				return err
			}
		}
		if _, err := stdout.Write([]byte(chat.RenderTelegramMarkdown(string(input)))); err != nil {
			return err
		}
	}
	return nil
}
