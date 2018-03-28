package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/mitchellh/mapstructure"
	qrcode "github.com/skip2/go-qrcode"
	irc "gopkg.in/sorcix/irc.v2"
)

type Connection struct {
	Chats []*Chat

	nickname  string
	number    string
	welcommed bool

	bridge *Bridge
	socket *net.TCPConn

	waitch chan bool
}

func MakeConnection() (*Connection, error) {
	return &Connection{
		bridge: MakeBridge(),
		waitch: make(chan bool),
	}, nil
}

func (conn *Connection) BindSocket(socket *net.TCPConn) error {
	conn.socket = socket
	write := conn.writeIRC

	welcome := func() {
		if conn.welcommed ||
			conn.nickname == "" || conn.number == "" {
			return
		}

		conn.writeIRC(fmt.Sprintf(":whapp-irc 001 %s Welcome to whapp-irc, %s.", conn.nickname, conn.nickname))
		conn.writeIRC(fmt.Sprintf(":whapp-irc 002 %s Enjoy the ride.", conn.nickname))

		conn.welcommed = true
	}

	status := func(msg string) error {
		return conn.writeIRC(fmt.Sprintf(":status PRIVMSG %s :%s", conn.nickname, msg))
	}

	conn.bridge.Start()

	go func() {
		decoder := irc.NewDecoder(bufio.NewReader(socket))
		for {
			msg, err := decoder.Decode()
			if err != nil {
				log.Printf("%#v", err)
				close(conn.waitch)
				return
			}
			fmt.Printf("%s\t%#v\n", conn.nickname, msg)

			switch msg.Command {
			case "NICK":
				conn.nickname = msg.Params[0]
				welcome()
			case "PASS":
				conn.number = msg.Params[0]
				welcome()

			case "CAP":
				write(":whapp-irc CAP * " + msg.Params[0])
			case "PING":
				write(":whapp-irc PONG whapp-irc :" + msg.Params[0])

			case "PRIVMSG":
				to := msg.Params[0]
				msg := msg.Params[1]

				if to == "status" {
					if msg[0] == '`' && msg[len(msg)-1] == '`' {
						cmd := msg[1 : len(msg)-1]
						conn.bridge.Write(Command{
							Command: "eval",
							Args:    []string{cmd},
						})
						continue
					}
					fmt.Printf("%s->status: %s\n", conn.nickname, msg)
					continue
				}

				chat := conn.GetChatByIdentifier(to)
				if chat == nil {
					status("unknown chat")
					continue
				}

				cid := chat.ID
				err := conn.bridge.Write(Command{
					Command: "send",
					Args:    []string{cid, msg},
				})
				if err != nil {
					fmt.Printf("err while sending %s\n", err.Error())
				}

			case "JOIN":
				chat := conn.GetChatByIdentifier(msg.Params[0])
				if chat == nil {
					status("chat not found")
					continue
				}
				err := conn.joinChat(chat)
				if err != nil {
					status("error while joining: " + err.Error())
				}

			case "PART":
				chat := conn.GetChatByIdentifier(msg.Params[0])
				if chat == nil {
					status("unknown chat")
					continue
				}

				// TODO: some way that we don't rejoin a person later.
				chat.Joined = false
			}
		}
	}()

	go func() {
		for {
			event, ok := <-conn.bridge.Chan
			if !ok {
				fmt.Printf("binding channel broke\n")
				close(conn.waitch)
				return
			}

			switch event.Event {
			case "qr":
				code := event.Args[0]["code"].(string)
				bytes, err := qrcode.Encode(code, qrcode.High, 512)
				if err != nil {
					panic(err) // REVIEW
				}
				path, err := fs.AddBlob("qr-"+strTimestamp(), bytes)
				if err != nil {
					panic(err) // REVIEW
				}
				status("Scan this QR code: " + path)

			case "ok":
				status("ok! id=" + event.Args[0]["id"].(string))

			case "chat":
				var chat *Chat
				mapstructure.Decode(event.Args[0], &chat)

				for id := range chat.Participants {
					contact := &chat.Participants[id]
					isAdmin := false
					for _, admin := range chat.Admins {
						if admin.ID == contact.ID {
							isAdmin = true
							break
						}
					}
					contact.IsAdmin = isAdmin
				}

				conn.Chats = append(conn.Chats, chat)
				fmt.Printf("%s\t\t\t\t%d participants\n", chat.Identifier(), len(chat.Participants))

			case "unread-messages":
				var msgGroups []MessageGroup
				mapstructure.Decode(event.Args, &msgGroups)
				for _, group := range msgGroups {
					chat := conn.GetChatByID(group.Chat.ID)

					if chat.IsGroupChat() && !chat.Joined {
						conn.joinChat(chat)
					}

					messages := group.Messages
					for _, msg := range messages { // HACK
						senderSafeName := msg.Sender.SafeName()
						message := msg.Content

						own := msg.Own(conn.number)
						if own {
							senderSafeName = conn.nickname
						}

						var to string
						if chat.IsGroupChat() || own {
							to = chat.Identifier()
						} else {
							to = conn.nickname
						}

						if msg.Filename != "" {
							bytes, err := base64.StdEncoding.DecodeString(msg.Content)
							if err != nil {
								fmt.Printf("err base64 %s\n", err.Error())
								continue
							}
							message, err = fs.AddBlob(getFileName(bytes), bytes)
							if err != nil {
								fmt.Printf("err addblob %s\n", err.Error())
								continue
							}

							if msg.Caption != "" {
								message += " " + msg.Caption
							}
						}

						for _, line := range strings.Split(message, "\n") {
							fmt.Printf("\t%s: %s\n", msg.Sender.FullName(), line)
							str := fmt.Sprintf(":%s PRIVMSG %s :%s", senderSafeName, to, line)
							write(str)
						}
					}
				}
			}
		}
	}()

	<-conn.waitch
	return nil
}

func (conn *Connection) joinChat(chat *Chat) error {
	if chat == nil {
		return fmt.Errorf("chat is nil")
	} else if !chat.IsGroupChat() {
		return fmt.Errorf("not a group chat")
	} else if chat.Joined {
		return nil
	}

	identifier := chat.Identifier()

	conn.writeIRC(fmt.Sprintf(":%s JOIN %s", conn.nickname, identifier))
	conn.writeIRC(fmt.Sprintf(":whapp-irc 332 %s %s :%s", conn.nickname, identifier, chat.Name))

	names := make([]string, 0)
	for _, contact := range chat.Participants {
		if contact.Self(conn.number) {
			if contact.IsAdmin {
				conn.writeIRC(fmt.Sprintf(":whapp-irc MODE %s +o %s", identifier, conn.nickname))
			}
			continue
		}

		prefix := ""
		if contact.IsAdmin {
			prefix = "@"
		}

		names = append(names, prefix+contact.SafeName())
	}

	conn.writeIRC(fmt.Sprintf(":whapp-irc 353 %s @ %s :%s", conn.nickname, identifier, strings.Join(names, " ")))
	conn.writeIRC(fmt.Sprintf(":whapp-irc 366 %s %s :End of /NAMES list.", conn.nickname, identifier))

	chat.Joined = true
	return nil
}

func (conn *Connection) writeIRC(msg string) error {
	bytes := []byte(msg + "\n")

	n, err := conn.socket.Write(bytes)
	if err != nil {
		return err
	} else if n != len(bytes) {
		return fmt.Errorf("bytes length mismatch")
	}

	return nil
}

func (conn *Connection) GetChatByID(ID string) *Chat {
	for _, c := range conn.Chats {
		if c.ID == ID {
			return c
		}
	}
	return nil
}

func (conn *Connection) GetChatByIdentifier(identifier string) *Chat {
	for _, c := range conn.Chats {
		if c.Identifier() == identifier {
			return c
		}
	}
	return nil
}