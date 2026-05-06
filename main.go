package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const version = "0.1.0"

type Config struct {
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
}

type AgentResponse struct {
	Success bool            `json:"success"`
	Tool    any             `json:"tool"`
	Data    json.RawMessage `json:"data"`
	Error   *AgentError     `json:"error"`
	CallID  any             `json:"call_id"`
}

type AgentError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "login":
		err = login(os.Args[2:])
	case "logout":
		err = logout(os.Args[2:])
	case "whoami":
		err = getAndPrint("/api/agent/v1/whoami", os.Args[2:])
	case "tools":
		err = getAndPrint("/api/agent/v1/tools", os.Args[2:])
	case "call":
		err = call(os.Args[2:])
	default:
		usage()
		err = fmt.Errorf("commande inconnue: %s", os.Args[1])
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`brico CLI

Commandes:
  brico login --base-url http://localhost:8000 [--source brico-cli]
  brico logout
  brico whoami
  brico tools
  brico call client.search --json '{"query":"dupont"}'
  brico call article.search --json '{"query":"courroie"}'
  brico version`)
}

func login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	baseURL := fs.String("base-url", "", "URL de l'intranet Laravel")
	source := fs.String("source", "brico-cli", "Origine de la demande")
	deviceName := fs.String("device-name", hostname(), "Nom de la machine")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, _ := loadConfig()
	if *baseURL == "" {
		*baseURL = cfg.BaseURL
	}
	if *baseURL == "" {
		return errors.New("--base-url est obligatoire au premier login")
	}

	startPayload := map[string]any{
		"source":      *source,
		"device_name": *deviceName,
	}
	body, err := request("POST", *baseURL, "/api/agent/v1/login/start", "", startPayload)
	if err != nil {
		return err
	}

	var start struct {
		Success bool `json:"success"`
		Data    struct {
			DeviceCode      string `json:"device_code"`
			UserCode        string `json:"user_code"`
			VerificationURL string `json:"verification_url"`
			Interval        int    `json:"interval"`
		} `json:"data"`
		Error *AgentError `json:"error"`
	}
	if err := json.Unmarshal(body, &start); err != nil {
		return err
	}
	if !start.Success {
		return responseError(start.Error)
	}

	fmt.Printf("Ouvre cette URL pour connecter la CLI:\n%s\n\n", start.Data.VerificationURL)
	_ = openBrowser(start.Data.VerificationURL)
	fmt.Println("En attente de validation...")

	interval := time.Duration(start.Data.Interval) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}

	for {
		time.Sleep(interval)
		pollPayload := map[string]any{"device_code": start.Data.DeviceCode}
		body, err := request("POST", *baseURL, "/api/agent/v1/login/poll", "", pollPayload)
		if err != nil {
			return err
		}

		var poll struct {
			Success bool `json:"success"`
			Data    struct {
				Token string `json:"token"`
			} `json:"data"`
			Error *AgentError `json:"error"`
		}
		if err := json.Unmarshal(body, &poll); err != nil {
			return err
		}
		if poll.Success {
			cfg.BaseURL = strings.TrimRight(*baseURL, "/")
			cfg.Token = poll.Data.Token
			if err := saveConfig(cfg); err != nil {
				return err
			}
			fmt.Println("Connecté.")
			return nil
		}
		if poll.Error == nil || poll.Error.Code != "authorization_pending" {
			return responseError(poll.Error)
		}
	}
}

func logout(args []string) error {
	cfg, err := loadRequiredConfig()
	if err != nil {
		return err
	}
	body, err := request("POST", cfg.BaseURL, "/api/agent/v1/logout", cfg.Token, map[string]any{})
	if err != nil {
		return err
	}
	if err := printResponse(body); err != nil {
		return err
	}
	cfg.Token = ""
	return saveConfig(cfg)
}

func getAndPrint(path string, args []string) error {
	cfg, err := loadRequiredConfig()
	if err != nil {
		return err
	}
	body, err := request("GET", cfg.BaseURL, path, cfg.Token, nil)
	if err != nil {
		return err
	}
	return printResponse(body)
}

func call(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: brico call <tool> --json '<payload>'")
	}

	tool := args[0]
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	jsonPayload := fs.String("json", "{}", "Payload JSON de la tool")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	var input map[string]any
	if err := json.Unmarshal([]byte(*jsonPayload), &input); err != nil {
		return fmt.Errorf("payload JSON invalide: %w", err)
	}

	cfg, err := loadRequiredConfig()
	if err != nil {
		return err
	}

	body, err := request("POST", cfg.BaseURL, "/api/agent/v1/call", cfg.Token, map[string]any{
		"tool":  tool,
		"input": input,
	})
	if err != nil {
		return err
	}
	return printResponse(body)
}

func request(method string, baseURL string, path string, token string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		_ = printResponse(data)
		return nil, fmt.Errorf("requête refusée: HTTP %d", resp.StatusCode)
	}

	return data, nil
}

func printResponse(body []byte) error {
	var response AgentResponse
	if err := json.Unmarshal(body, &response); err != nil {
		fmt.Println(string(body))
		return nil
	}

	pretty, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(pretty))

	if !response.Success {
		return responseError(response.Error)
	}
	return nil
}

func responseError(agentError *AgentError) error {
	if agentError == nil {
		return errors.New("appel échoué")
	}
	return fmt.Errorf("%s: %s", agentError.Code, agentError.Message)
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "brico", "config.json"), nil
}

func loadConfig() (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	return cfg, json.Unmarshal(data, &cfg)
}

func loadRequiredConfig() (Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return Config{}, errors.New("non connecté: lance `brico login --base-url ...`")
	}
	if cfg.BaseURL == "" || cfg.Token == "" {
		return Config{}, errors.New("non connecté: lance `brico login --base-url ...`")
	}
	return cfg, nil
}

func saveConfig(cfg Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}
