package server

import (
	"errors"
	"fmt"
	"github.com/wavyllama/chat/db"
	"github.com/wavyllama/chat/protocol"
	"log"
	"net"
	"time"
	"encoding/gob"
	"encoding/json"
	"github.com/wavyllama/chat/core"
)

const (
	Port    uint16 = 4242
	Network        = "tcp"
)

// Server holds the user and all of his sessions
type Server struct {
	User     *db.User
	Listener *net.TCPListener
	Sessions *[]Session
}

func init() {
	gob.Register(&FriendMessage{})
	gob.Register(&HandshakeMessage{})
	gob.Register(&ChatMessage{})
}

// Setup listener for the server
func setupServer(address string) (*net.TCPListener, error) {
	tcpAddr, err := net.ResolveTCPAddr(Network, address)
	if err != nil {
		return nil, err
	}
	return net.ListenTCP(Network, tcpAddr)
}

// Handle receiving messages from a TCPConn
func (s *Server) handleConnection(conn *net.TCPConn) {
	defer conn.Close()
	decoder := gob.NewDecoder(conn)
	var msg Message
	if err := decoder.Decode(&msg); err != nil {
		log.Panicf("Error decoding message: %s", err.Error())
	}
	res, _ := json.Marshal(msg)
	fmt.Printf("RECEIVED MESSAGE: %s\n", string(res))

	sourceIP := conn.RemoteAddr().(*net.TCPAddr).IP.String()
	sourceMAC, sourceUsername := msg.SourceID()
	if sourceMAC == "" || sourceUsername == "" {
		log.Panicln("Received ill-formatted message")
	}
	if msg.DestID() != s.User.Username {
		fmt.Println("Received a message but it was not for me.")
		return
	}
	messageYourself := sourceMAC == s.User.MAC && sourceUsername == s.User.Username
	sessions := s.GetSessionsWithFriend(sourceMAC, sourceUsername)
	friend := s.User.GetFriendByUsernameAndMAC(sourceUsername, sourceMAC)

	switch msg.(type) {
	case *FriendMessage:
		if friend == nil {
			// TODO: stop listener server in week 4
			fmt.Printf("You received a friend request from %s at %s\n", sourceUsername, sourceIP)
			var friendDisplayName string
			for {
				friendDisplayName = core.GetDisplayNameFromConsole()
				if s.User.IsFriendsWith(friendDisplayName) {
					fmt.Printf("You already have a friend named '%s'\n", friendDisplayName)
					continue
				}
				break
			}
			// TODO: start listener server again in week 4
			s.User.AddFriend(friendDisplayName, sourceMAC, sourceIP, sourceUsername)

			s.SendFriendRequest(sourceIP, sourceUsername)
		}
	case *HandshakeMessage:
		// We are in a handshake, so the friend should exist already
		if friend == nil {
			log.Panicln("You must be a friend to participate in a handshake")
		}
		var createdSession bool
		var sess Session
		round := msg.(*HandshakeMessage).Round
		protoType, startSessionTime := msg.(*HandshakeMessage).ProtoType, msg.(*HandshakeMessage).SessionTime

		// In a handshake, create a new session if there aren't the required number of sessions in either situation
		if len(sessions) != 2 && messageYourself || (len(sessions) != 1 && !messageYourself) {
			sess = *NewSessionFromUserAndMessage(s.User, friend, protoType, startSessionTime)
			*(*s).Sessions = append(*(*s).Sessions, sess)
			createdSession = true
		} else if len(sessions) == 2 && messageYourself {
			// Communicating between yourself, rotate sessions based on round (even/odd)
			sess = sessions[round % 2]
		} else {
			sess = sessions[0]
		}

		dec, err := sess.Proto.Decrypt(msg.(*HandshakeMessage).Secret)

		switch errorType := err.(type) {
		case protocol.OTRHandshakeStep:
			// Send each part of the handshake message back and immediately return
			for _, stepMessage := range dec {
				reply := new(HandshakeMessage)
				reply.NewPayload(s.User.MAC, s.User.Username, sourceUsername)
				reply.Secret = stepMessage
				reply.ProtoType = msg.(*HandshakeMessage).ProtoType
				// If we created a session here, then set current time as start time
				if createdSession {
					reply.SessionTime = time.Now()
				}
				reply.Round = round + 1
				s.sendMessage(sourceIP, reply)
			}
			return
		default:
			// another type of error, which means err is probably not nil
			if err != nil {
				log.Panicf("ReceiveMessage: %s, Error Type: %s", err.Error(), errorType)
			}
		}
	case *ChatMessage:
		var sess Session
		// There are two sessions, so grab the one that doesn't have the same timestamp as you
		if messageYourself {
			sess = sessions[1]
		} else {
			// There should only be one session between A -> B if you aren't messaging yourself, so grab that
			sess = sessions[0]
		}
		dec, _ := sess.Proto.Decrypt(msg.(*ChatMessage).Text)
		if sess.Proto.IsActive() && dec[0] != nil {
			// Print the decoded message and IP
			fmt.Printf("%s: %s\n", friend.DisplayName, dec[0])
		}
	}
}

// Function that continuously polls for new messages being sent to the server
func (s *Server) receive() {
	for {
		if conn, err := (*(*s).Listener).AcceptTCP(); err == nil {
			go s.handleConnection(conn)
		}
	}
}

func initDialer(address string) (*net.TCPConn, error) {
	tcpAddr, err := net.ResolveTCPAddr(Network, address)
	if err != nil {
		return nil, err
	}
	return net.DialTCP(Network, nil, tcpAddr)
}

// Start up server
func (s *Server) Start(username string, mac string, ip string) error {
	var err error
	log.Println("Launching Server...")
	(*s).User = &db.User{username, mac, ip}
	ipAddr := fmt.Sprintf("%s:%d", ip, Port)
	if (*s).Listener, err = setupServer(ipAddr); err != nil {
		return err
	}
	// Initialize the session struct to a pointer
	(*s).Sessions = &[]Session{}
	go s.receive()
	log.Printf("Listening on: '%s:%d'", ip, Port)

	// Updates the IP address of the user and create a friend for yourself
	if s.User.GetFriendByDisplayName(core.Self) == nil {
		s.User.AddFriend(core.Self, mac, ip, username)
	}

	s.User.UpdateMyIP()
	s.StartSession(core.Self, protocol.OTRProtocol{})

	return nil
}

// End server connection
func (s *Server) Shutdown() error {
	log.Println("Shutting Down Server...")
	return (*s).Listener.Close()
}

// Sends a formatted Message object with the server, after an active session between the two users have been established
func (s *Server) sendMessage(destIp string, msg Message) error {
	dialer, err := initDialer(fmt.Sprintf("%s:%d", destIp, Port))
	if err != nil {
		return err
	}

	encoder := gob.NewEncoder(dialer)
	if err := encoder.Encode(&msg); err != nil {
		return err
	}
	return nil
}

// Get all sessions that a user talks to an IP
// There are only 2 if a user is talking to himself
// otherwise only 1 session is returned
func (s *Server) GetSessionsWithFriend(friendMAC string, friendUsername string) []Session {
	var filterSessions []Session
	for _, sess := range *(*s).Sessions {
		if sess.To.MAC == friendMAC && sess.To.Username == friendUsername {
			filterSessions = append(filterSessions, sess)
		}
	}
	return filterSessions
}

// Start a session with a destination IP using a protocol
func (s *Server) StartSession(displayName string, proto protocol.Protocol) error {
	friend := s.User.GetFriendByDisplayName(displayName)
	if friend == nil {
		fmt.Printf("You do not have a friend named '%s'\n", displayName)
	}
	sessions := s.GetSessionsWithFriend(friend.MAC, friend.Username)
	if len(sessions) != 0 {
		return nil
	}

	firstMessage, err := proto.NewSession()
	if err != nil {
		log.Panicf("StartSession: Error starting new session: %s", err)
		return err
	}

	msg := new(HandshakeMessage)
	msg.NewPayload(s.User.MAC, s.User.Username, friend.Username)
	msg.Secret = []byte(firstMessage)
	msg.ProtoType = proto.ToType()
	msg.Round = 0
	return s.sendMessage(friend.IP, msg)
}

// Sends a friend request to a specified destUsername@destIP
func (s *Server) SendFriendRequest(destIP, destUsername string) error {
	friendRequest := new(FriendMessage)
	friendRequest.NewPayload(s.User.MAC, s.User.Username, destUsername)

	return s.sendMessage(destIP, friendRequest)
}

// Sends a chat message based on friend display name
func (s *Server) SendChatMessage(friendDisplayName, message string) error {
	chatMsg := new(ChatMessage)

	friend := s.User.GetFriendByDisplayName(friendDisplayName)
	if friend == nil {
		return errors.New(fmt.Sprintf("Friend with display name '%s' does not exist", friendDisplayName))
	}
	sessions := s.GetSessionsWithFriend(friend.MAC, friend.Username)
	if len(sessions) == 0 {
		return errors.New(fmt.Sprintf("Cannot communicate with '%s' without an active session\n", friendDisplayName))
	}
	cyp, err := sessions[0].Proto.Encrypt(chatMsg.Text)
	if err != nil {
		return err
	}
	(*chatMsg).Text = cyp[0]

	chatMsg.NewPayload(s.User.MAC, s.User.Username, friend.Username)
	(*chatMsg).Text = []byte(message)
	return s.sendMessage(friend.IP, chatMsg)
}