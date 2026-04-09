package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func initCmd(configPath string) {
	scanner := bufio.NewScanner(os.Stdin)

	prompt := func(label, defaultVal string) string {
		if defaultVal != "" {
			fmt.Printf("%s [%s]: ", label, defaultVal)
		} else {
			fmt.Printf("%s: ", label)
		}
		if !scanner.Scan() {
			os.Exit(0)
		}
		val := strings.TrimSpace(scanner.Text())
		if val == "" {
			return defaultVal
		}
		return val
	}

	nodeName := prompt("Node name", "my-node")

	fmt.Println()
	fmt.Println("Role:")
	fmt.Println("  1. client  — Connect to remote SSH servers, set up tunnels")
	fmt.Println("  2. server  — Accept incoming SSH connections")
	fmt.Println("  3. both    — Client + server on the same node")
	roleChoice := prompt("Choose role (1/2/3)", "1")

	var role string
	switch roleChoice {
	case "2", "server":
		role = "server"
	case "3", "both":
		role = "both"
	default:
		role = "client"
	}

	var listenAddr, hostKeyPath, authorizedKeysPath string
	if role == "server" || role == "both" {
		listenAddr = prompt("SSH listen address", "0.0.0.0:2222")
		hostKeyPath = prompt("Host key path", "~/.ssh/id_ed25519")
		authorizedKeysPath = prompt("Authorized keys path", "~/.ssh/authorized_keys")
	}

	var targetHost, identityFile, knownHostsPath string
	if role == "client" || role == "both" {
		targetHost = prompt("SSH target (user@host:port)", "root@example.com:22")
		identityFile = prompt("Private key path", "~/.ssh/id_ed25519")
		knownHostsPath = prompt("Known hosts path", "~/.ssh/known_hosts")
	}

	// Build YAML
	var b strings.Builder
	fmt.Fprintf(&b, "# yaml-language-server: $schema=https://raw.githubusercontent.com/mmdemirbas/mesh/main/configs/mesh.schema.json\n\n")
	fmt.Fprintf(&b, "%s:\n", nodeName)
	fmt.Fprintf(&b, "  log_level: info\n")
	fmt.Fprintf(&b, "  admin_addr: \"127.0.0.1:7777\"\n")

	if role == "server" || role == "both" {
		fmt.Fprintf(&b, "\n  listeners:\n")
		fmt.Fprintf(&b, "    - name: ssh-server\n")
		fmt.Fprintf(&b, "      type: sshd\n")
		fmt.Fprintf(&b, "      bind: \"%s\"\n", listenAddr)
		fmt.Fprintf(&b, "      host_key: \"%s\"\n", hostKeyPath)
		fmt.Fprintf(&b, "      authorized_keys: \"%s\"\n", authorizedKeysPath)
	}

	if role == "client" || role == "both" {
		fmt.Fprintf(&b, "\n  connections:\n")
		fmt.Fprintf(&b, "    - name: tunnel\n")
		fmt.Fprintf(&b, "      targets:\n")
		fmt.Fprintf(&b, "        - \"%s\"\n", targetHost)
		fmt.Fprintf(&b, "      options:\n")
		fmt.Fprintf(&b, "        IdentityFile: \"%s\"\n", identityFile)
		if knownHostsPath != "" {
			fmt.Fprintf(&b, "        UserKnownHostsFile: \"%s\"\n", knownHostsPath)
		}
		fmt.Fprintf(&b, "      forwards:\n")
		fmt.Fprintf(&b, "        - name: default\n")
		fmt.Fprintf(&b, "          local:\n")
		fmt.Fprintf(&b, "            - bind: \"127.0.0.1:8080\"\n")
		fmt.Fprintf(&b, "              target: \"127.0.0.1:80\"\n")
	}

	yaml := b.String()

	fmt.Println()
	fmt.Println("─── Generated config ───")
	fmt.Println(yaml)
	fmt.Println("────────────────────────")

	// Determine output path
	if configPath == "" || configPath == "mesh.yaml" {
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".mesh", "conf")
		_ = os.MkdirAll(dir, 0750)
		configPath = filepath.Join(dir, "mesh.yaml")
	}

	// Check if file already exists
	if _, err := os.Stat(configPath); err == nil {
		overwrite := prompt(fmt.Sprintf("File %s already exists. Overwrite? (y/N)", configPath), "n")
		if !strings.EqualFold(overwrite, "y") {
			fmt.Println("Aborted.")
			return
		}
	}

	if err := os.WriteFile(configPath, []byte(yaml), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nConfig written to %s\n", configPath)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  mesh -f %s up       # Start the node\n", configPath)
	fmt.Printf("  mesh -f %s status   # Check node status\n", configPath)
	fmt.Printf("  mesh -f %s config   # View parsed config\n", configPath)
}
