package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nlopes/slack"
	"golang.org/x/crypto/ssh"
)

var tunnelOnTime time.Time
var tunnelIdleSince time.Time
var maxTunnelIdleTime = float64(5 * 60) // 5 mins in seconds

func executeRemoteCmd(command string) string {
	defer func() { //catch or finally
		if err := recover(); err != nil { //catch
			fmt.Fprintf(os.Stderr, "Exception: %v\n", err)
		}
	}()

	config := &ssh.ClientConfig{
		User: os.Getenv("piUser"),
		Auth: []ssh.AuthMethod{ssh.Password(os.Getenv("piPass"))},
	}

	if raspberryPIIP == "" {
		raspberryPIIP = "raspberrypi.fritz.box"
	}
	connectionString := fmt.Sprintf("%s:%s", raspberryPIIP, "22")
	conn, errConn := ssh.Dial("tcp", connectionString, config)
	if errConn != nil { //catch
		fmt.Fprintf(os.Stderr, "Exception: %v\n", errConn)
	}
	session, _ := conn.NewSession()
	defer session.Close()

	var stdoutBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	var stderrBuf bytes.Buffer // hmmm
	session.Stderr = &stderrBuf
	session.Run(command)

	tunnelIdleSince = time.Now()

	errors := ""
	if stderrBuf.String() != "" {
		errStr := strings.TrimSpace(stderrBuf.String())
		errors = "ERR `" + errStr + "`"
	}

	return stdoutBuf.String() + errors
}

// RaspberryPIPrivateTunnelChecks ensures PrivateTunnel vpn connection
// on PI is up and working properly
func RaspberryPIPrivateTunnelChecks(userCall bool) string {
	tunnelUp := ""
	response := ":openvpn: PI status: DOWN :rotating_light:"

	if runningFritzboxTunnel() {
		// `curl ipinfo.io` (if this doesn't work, just `curl icanhazip.com`)
		results := make(chan string, 10)
		timeout := time.After(5 * time.Second)
		go func() {
			// TODO: get both ipv4+ipv6 internet addresses
			/*
			curl https://ipleak.net/json/
			*/
			results <- executeRemoteCmd("https://ipleak.net/json/")
		}()

		type IPInfoResponse struct {
			IP      string
			CountryCode string
		}
		var jsonRes IPInfoResponse

		select {
		case res := <-results:
			if res != "" {
				err := json.Unmarshal([]byte(res), &jsonRes)
				if err != nil {
					fmt.Printf("unable to parse JSON string %s\n", res)
				}
				if jsonRes.CountryCode == "NL" {
					resultsDig := make(chan string, 10)
					timeoutDig := time.After(5 * time.Second)
					// ensure home.ackerson.de is DIFFERENT than PI IP address!
					go func() {
						resultsDig <- executeRemoteCmd("dig home.ackerson.de A home.ackerson.de AAAA +short")
					}()
					select {
					case resComp := <-resultsDig:
						// TODO: ipv4 + ipv6 means multiline parse
						/*
						  ackerson.dynv6.net.
							93.227.143.65
							ackerson.dynv6.net.
							2003:dc:d3bf:20f0:a96:d7ff:fe12:7ee7
						*/
						if resComp != jsonRes.IP {
							tunnelUp = jsonRes.IP
						}
					case <-timeoutDig:
						fmt.Println("Timed out on dig home.ackerson.de!")
					}
				}
			}
		case <-timeout:
			fmt.Println("Timed out on curl ipinfo.io!")
		}

		// Tunnel should be OK. Now double check iptables to ensure that
		// ALL Internet requests are running over OpenVPN!
		if tunnelUp != "" {
			resultsIPTables := make(chan string, 10)
			timeoutIPTables := time.After(5 * time.Second)
			// ensure home.ackerson.de is DIFFERENT than PI IP address!
			go func() {
				resultsIPTables <- executeRemoteCmd("sudo iptables -L OUTPUT -v --line-numbers | grep all")
			}()
			select {
			case resIPTables := <-resultsIPTables:
				lines := strings.Split(resIPTables, "\n")

				for idx, oneLine := range lines {
					switch idx {
					case 0:
						if !strings.Contains(oneLine, "ACCEPT     all  --  any    tun0    anywhere") {
							tunnelUp = ""
						}
					case 1:
						if !strings.Contains(oneLine, "ACCEPT     all  --  any    eth0    anywhere             192.168.178.0/24") {
							tunnelUp = ""
						}
					case 2:
						if !strings.Contains(oneLine, "DROP       all  --  any    eth0    anywhere             anywhere") {
							tunnelUp = ""
						}
					}
				}

				// TODO: ip6tables -L OUTPUT -v --line-numbers
				/*
1        0     0 DROP       all      any    any     anywhere             anywhere             rt type:0 segsleft:0
2      161 20197 ACCEPT     all      any    any     fe80::/10            anywhere
3       23  2830 ACCEPT     all      any    any     anywhere             ff00::/8
5       19  2311 ACCEPT     all      any    lo      anywhere             anywhere
6    41090 3333K ACCEPT     all      any    tun0    anywhere             anywhere
7     1211  624K ACCEPT     all      any    eth0    anywhere             p200300DCD3E0EF000000000000000000.dip0.t-ipconnect.de/64
15      74 21413 DROP       all      any    eth0    anywhere             anywhere
				*/
				// verify line 6: ACCEPT tun0 anywhere anywhere
				// verify line 7: ACCEPT eth0 anywhere p2003*
				// verify line 15: DROP eth0 anywhere anywhere
			case <-timeoutIPTables:
				fmt.Println("Timed out on `iptables -L OUTPUT`!")
			}
			//  TODO if tunnelUp = "" shutdown transmission daemon, restart VPN and send RED ALERT msg!
		}

		if tunnelUp != "" {
			response = ":openvpn: PI status: UP :raspberry_pi: @ " + tunnelUp
		}

		if !userCall {
			customEvent := slack.RTMEvent{Type: "RaspberryPIPrivateTunnelChecks", Data: response}
			rtm.IncomingEvents <- customEvent
		}
	} else {
		response = "Unable to connect to Fritz!Box tunnel to check :openvpn:"
	}
	return response
}

// CheckPiDiskSpace now exported
func CheckPiDiskSpace(path string) string {
	userCall := true
	if path == "---" {
		path = ""
		userCall = false
	}

	response := executeRemoteCmd("du -sh \""+piSDCardPath+path+"\"/*") + "\n\n"
	response += executeRemoteCmd("df -h /root/")

	if !userCall {
		customEvent := slack.RTMEvent{Type: "CheckPiDiskSpace", Data: response}
		rtm.IncomingEvents <- customEvent
	}

	return response
}

// DeleteTorrentFile now exported
func DeleteTorrentFile(filename string) string {
	response := ""
	if filename == "*" || filename == "" || strings.Contains(filename, "../") {
		response = "Please enter an existing filename - try `fsck`"
	} else {
		path := piSDCardPath + filename

		deleteCmd := ""
		isDir := executeRemoteCmd("test -d \"" + path + "\" && echo 'Yes'")
		if strings.HasPrefix(isDir, "Yes") {
			deleteCmd = "rm -Rf \"" + path + "\""
		} else {
			deleteCmd = "rm \"" + path + "\""
		}

		response = executeRemoteCmd(deleteCmd)
	}

	return response
}

// MoveTorrentFile now exported
func MoveTorrentFile(filename string) {
	if filename == "*" || filename == "" || strings.Contains(filename, "../") {
		rtm.IncomingEvents <- slack.RTMEvent{Type: "MoveTorrent", Data: "Please enter an existing filename - try `fsck`"}
	} else {
		moveCmd := "mv \"" + piSDCardPath + filename + "\" " + piUSBMountPath

		go func() {
			result := executeRemoteCmd(moveCmd)
			fmt.Printf("mv ? %v", result)
			if result == "" {
				result = "Successfully moved " + filename + " to " + piUSBMountPath
			}
			rtm.IncomingEvents <- slack.RTMEvent{Type: "MoveTorrent", Data: result}
		}()
	}
}

// DisconnectIdleTunnel is now commented
func DisconnectIdleTunnel() {
	msg := ":closed_lock_with_key: UP since: " + tunnelOnTime.Format("Mon, Jan 2 15:04") + " IDLE for "

	if !tunnelOnTime.IsZero() {
		currentIdleTime := time.Now().Sub(tunnelIdleSince)
		stringCurrentIdleTimeSecs := strconv.FormatFloat(currentIdleTime.Seconds(), 'f', 0, 64)
		if currentIdleTime.Seconds() > maxTunnelIdleTime {
			vpnTunnelCmds("/usr/sbin/vpnc-disconnect")
			msg += stringCurrentIdleTimeSecs + "secs => disconnected!"
		} else {
			msg += stringCurrentIdleTimeSecs + "secs"
		}

		rtm.SendMessage(rtm.NewOutgoingMessage(msg, SlackReportChannel))
	}
}

func vpnTunnelCmds(command ...string) string {
	if command[0] != "status" {
		cmd := exec.Command(command[0])

		args := len(command)
		if args > 1 {
			cmd = exec.Command(command[0], command[1])
		}

		errStart := cmd.Start()
		if errStart != nil {
			os.Stderr.WriteString(errStart.Error())
		} else if errWait := cmd.Wait(); errWait != nil {
			os.Stderr.WriteString(errWait.Error())
		}

		if strings.HasSuffix(command[0], "vpnc-connect") {
			now := time.Now()
			tunnelOnTime, tunnelIdleSince = now, now
		} else if strings.HasSuffix(command[0], "vpnc-disconnect") {
			tunnelOnTime = *new(time.Time)
			tunnelIdleSince = *new(time.Time)
		}
	}

	/* Here's the next cmd to get setup
			# ip a show tun0
			9: tun0: <POINTOPOINT,MULTICAST,NOARP,UP,LOWER_UP> mtu 1024 qdisc
	    		inet 192.168.178.201/32 scope global tun0
	       	valid_lft forever preferred_lft forever
			# vpnc-disconnect
				Terminating vpnc daemon (pid: 174)
			# ip a show tun0
				Device "tun0" does not exist.
	*/
	tun0StatusCmd := "/sbin/ip a show tun0 | /bin/grep tun0 | /usr/bin/tail -1"
	tunnel, err := exec.Command("/bin/bash", "-c", tun0StatusCmd).Output()
	if err != nil {
		fmt.Printf("Failed to execute command: %s", tun0StatusCmd)
	}

	tunnelStatus := string(tunnel)
	if len(tunnelStatus) == 0 {
		tunnelStatus = "Tunnel offline."
	}

	return ":closed_lock_with_key: " + tunnelStatus + " IDLE since: " + tunnelIdleSince.Format("Mon Jan _2 15:04")
}

func runningFritzboxTunnel() bool {
	up := isFritzboxTunnelUp()

	if !up { // attempt to establish connection
		vpnTunnelCmds("/usr/sbin/vpnc-connect", "fritzbox")
		if up = isFritzboxTunnelUp(); !up {
			rtm.SendMessage(rtm.NewOutgoingMessage(
				":closed_lock_with_key: Unable to tunnel to Fritz!Box", ""))
		}
	}

	return up // if running locally, change to true
}

func isFritzboxTunnelUp() bool {
	status := false

	tunnelStatus := vpnTunnelCmds("status")
	if strings.Contains(tunnelStatus, "192.168.178.201") {
		status = true
	}

	return status
}
