package slack

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackutilsx"
	"github.com/target/goalert/config"
	"github.com/target/goalert/notification"
	"github.com/target/goalert/permission"
	"github.com/target/goalert/user"
	"github.com/target/goalert/util"
	"github.com/target/goalert/util/log"
	"github.com/target/goalert/validation"
)

type ChannelSender struct {
	cfg Config

	teamID string
	token  string

	chanCache *ttlCache
	listCache *ttlCache

	listMx sync.Mutex
	chanMx sync.Mutex
	teamMx sync.Mutex

	recv notification.Receiver
}

const (
	colorClosed  = "#218626"
	colorUnacked = "#862421"
	colorAcked   = "#867321"
)

var _ notification.Sender = &ChannelSender{}
var _ notification.ReceiverSetter = &ChannelSender{}

func NewChannelSender(ctx context.Context, cfg Config) (*ChannelSender, error) {
	return &ChannelSender{
		cfg: cfg,

		listCache: newTTLCache(250, time.Minute),
		chanCache: newTTLCache(1000, 15*time.Minute),
	}, nil
}

func (s *ChannelSender) SetReceiver(r notification.Receiver) {
	s.recv = r
}

// Channel contains information about a Slack channel.
type Channel struct {
	ID     string
	Name   string
	TeamID string
}

func rootMsg(err error) string {
	unwrapped := errors.Unwrap(err)
	if unwrapped == nil {
		return err.Error()
	}

	return rootMsg(unwrapped)
}

func mapError(ctx context.Context, err error) error {
	switch rootMsg(err) {
	case "channel_not_found":
		return validation.NewFieldError("ChannelID", "Invalid Slack channel ID.")
	case "missing_scope", "invalid_auth", "account_inactive", "token_revoked", "not_authed":
		log.Log(ctx, err)
		return validation.NewFieldError("ChannelID", "Permission Denied.")
	}

	return err
}

// Channel will lookup a single Slack channel for the bot.
func (s *ChannelSender) Channel(ctx context.Context, channelID string) (*Channel, error) {
	err := permission.LimitCheckAny(ctx, permission.User, permission.System)
	if err != nil {
		return nil, err
	}

	s.chanMx.Lock()
	defer s.chanMx.Unlock()
	res, ok := s.chanCache.Get(channelID)
	if !ok {
		ch, err := s.loadChannel(ctx, channelID)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		s.chanCache.Add(channelID, ch)
		return ch, nil
	}
	if err != nil {
		return nil, err
	}

	return res.(*Channel), nil
}

func (s *ChannelSender) TeamID(ctx context.Context) (string, error) {
	cfg := config.FromContext(ctx)

	s.teamMx.Lock()
	defer s.teamMx.Unlock()
	if s.teamID == "" || s.token != cfg.Slack.AccessToken {
		// teamID missing or token changed
		id, err := s.lookupTeamIDForToken(ctx, cfg.Slack.AccessToken)
		if err != nil {
			return "", err
		}

		// update teamID and token after fetching succeeds
		s.teamID = id
		s.token = cfg.Slack.AccessToken
	}

	return s.teamID, nil
}

func (s *ChannelSender) loadChannel(ctx context.Context, channelID string) (*Channel, error) {
	teamID, err := s.TeamID(ctx)
	if err != nil {
		return nil, fmt.Errorf("lookup team ID: %w", err)
	}

	ch := &Channel{TeamID: teamID}
	err = s.withClient(ctx, func(c *slack.Client) error {
		resp, err := c.GetConversationInfoContext(ctx, channelID, false)
		if err != nil {
			return err
		}

		ch.ID = resp.ID
		ch.Name = "#" + resp.Name

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("lookup conversation info: %w", err)
	}

	return ch, nil
}

// ListChannels will return a list of channels visible to the slack bot.
func (s *ChannelSender) ListChannels(ctx context.Context) ([]Channel, error) {
	err := permission.LimitCheckAny(ctx, permission.User, permission.System)
	if err != nil {
		return nil, err
	}

	cfg := config.FromContext(ctx)
	s.listMx.Lock()
	defer s.listMx.Unlock()
	res, ok := s.listCache.Get(cfg.Slack.AccessToken)
	if !ok {
		chs, err := s.loadChannels(ctx)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		ch2 := make([]Channel, len(chs))
		copy(ch2, chs)
		s.listCache.Add(cfg.Slack.AccessToken, ch2)
		return chs, nil
	}
	if err != nil {
		return nil, err
	}

	chs := res.([]Channel)
	cpy := make([]Channel, len(chs))
	copy(cpy, chs)

	return cpy, nil
}

func (s *ChannelSender) loadChannels(ctx context.Context) ([]Channel, error) {
	teamID, err := s.TeamID(ctx)
	if err != nil {
		return nil, fmt.Errorf("lookup team ID: %w", err)
	}

	n := 0
	var channels []Channel
	var cursor string
	for {
		n++
		if n > 10 {
			return nil, errors.New("abort after > 10 pages of Slack channels")
		}

		err = s.withClient(ctx, func(c *slack.Client) error {
			respChan, nextCursor, err := c.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
				ExcludeArchived: true,
				Types:           []string{"private_channel", "public_channel"},
				Limit:           200,
				Cursor:          cursor,
			})
			if err != nil {
				return err
			}

			cursor = nextCursor

			for _, ch := range respChan {
				channels = append(channels, Channel{
					ID:     ch.ID,
					Name:   "#" + ch.Name,
					TeamID: teamID,
				})
			}

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("list channels: %w", err)
		}

		if cursor == "" {
			break
		}
	}

	return channels, nil
}

func (s *ChannelSender) alertLink(ctx context.Context, id int, summary string, alertUsers []notification.User) string {
	teamID, err := s.TeamID(ctx)

  userIDs := make([]string, len(alertUsers))
  for i, u := range alertUsers {
    userIDs[i] = u.ID
  }

  userSlackIDs := make(map[string]string, len(alertUsers))
  err = s.cfg.UserStore.AuthSubjectsFunc(ctx, "slack:"+teamID, userIDs, func(sub user.AuthSubject) error {
    userSlackIDs[sub.UserID] = sub.SubjectID
    return nil
  })
  if err != nil {
    log.Log(ctx, fmt.Errorf("lookup auth subjects for slack: %w", err))
    // handled error by logging, continue on to render message with any included slack IDs
  }
	var userLinks []string
	for _, u := range alertUsers {
		var subjectID string
		if userSlackIDs != nil {
			subjectID = userSlackIDs[u.ID]
		}
		if subjectID == "" {
			// fallback to a link to the GoAlert user
			userLinks = append(userLinks, fmt.Sprintf("<%s|%s>", slackutilsx.EscapeMessage(u.URL), slackutilsx.EscapeMessage(u.Name)))
			continue
		}

		userLinks = append(userLinks, fmt.Sprintf("<@%s>", slackutilsx.EscapeMessage(subjectID)))
	}

	var users string
	if len(userLinks) == 0 {
		users = "None"
	}
	if len(userLinks) == 1 {
		users = userLinks[0]
	}
	if len(userLinks) == 2 {
		users = fmt.Sprintf("%s and %s", userLinks[0], userLinks[1])
	}
	if len(userLinks) > 2 {
		users = fmt.Sprintf("%s, and %s", strings.Join(userLinks[:len(userLinks)-1], ", "), userLinks[len(userLinks)-1])
	}

	cfg := config.FromContext(ctx)
	path := fmt.Sprintf("/alerts/%d", id)
	return fmt.Sprintf(`
<%s|Alert #%d: %s>
Personnel: %s
    `,
    cfg.CallbackURL(path),
    id,
    slackutilsx.EscapeMessage(summary),
    users,
  )
}

const (
	alertResponseBlockID = "block_alert_response"
	alertCloseActionID   = "action_alert_close"
	alertAckActionID     = "action_alert_ack"
)

// alertMsgOption will return the slack.MsgOption for an alert-type message (e.g., notification or status update).
func (s *ChannelSender) alertMsgOption(ctx context.Context, callbackID string, id int, summary string, users []notification.User, details, logEntry string, state notification.AlertState) slack.MsgOption {
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", s.alertLink(ctx, id, summary, users), false, false), nil, nil),
	}

	var color string
	var actions []slack.Block
	switch state {
	case notification.AlertStateAcknowledged:
		color = colorAcked
	case notification.AlertStateUnacknowledged:
		color = colorUnacked
	case notification.AlertStateClosed:
		color = colorClosed
		details = ""
	}
	if details != "" {
		escaped, err := util.RenderSize(3000, details, func(s string) (string, error) {
			return slackutilsx.EscapeMessage(s), nil
		})
		if err != nil {
			log.Log(ctx, fmt.Errorf("slack: render alert details: %w", err))
			escaped = ""
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", escaped, false, false), nil, nil),
		)
	}

	blocks = append(blocks,
		slack.NewContextBlock("", slack.NewTextBlockObject("plain_text", logEntry, false, false)),
	)
	cfg := config.FromContext(ctx)
	if len(actions) > 0 && cfg.Slack.InteractiveMessages {
		blocks = append(blocks, actions...)
	}

	return slack.MsgOptionAttachments(
		slack.Attachment{
			Color:    color,
			Fallback: fmt.Sprintf("Alert #%d: %s", id, slackutilsx.EscapeMessage(summary)),
			Blocks:   slack.Blocks{BlockSet: blocks},
		},
	)
}

func (s *ChannelSender) Send(ctx context.Context, msg notification.Message) (*notification.SentMessage, error) {

	cfg := config.FromContext(ctx)

	// Note: We don't use cfg.ApplicationName() here since that is configured in the Slack app as the bot name.

	var opts []slack.MsgOption
	var isUpdate bool
	switch t := msg.(type) {
	case notification.Alert:
		if t.OriginalStatus != nil {

			// Reply in thread if we already sent a message for this alert.
			opts = append(opts,
				slack.MsgOptionTS(t.OriginalStatus.ProviderMessageID.ExternalID),
				slack.MsgOptionText(s.alertLink(ctx, t.AlertID, t.Summary, t.Users), false),
			)
			break
		}

		opts = append(opts, s.alertMsgOption(ctx, t.CallbackID, t.AlertID, t.Summary, t.Users, t.Details, "Unacknowledged", notification.AlertStateUnacknowledged))
	case notification.AlertStatus:
		isUpdate = true
		opts = append(opts,
			slack.MsgOptionUpdate(t.OriginalStatus.ProviderMessageID.ExternalID),
			s.alertMsgOption(ctx, t.OriginalStatus.ID, t.AlertID, t.Summary, t.Users, t.Details, t.LogEntry, t.NewAlertState),
		)
	case notification.AlertBundle:
		opts = append(opts, slack.MsgOptionText(
			fmt.Sprintf("Service '%s' has %d unacknowledged alerts.\n\n<%s>", slackutilsx.EscapeMessage(t.ServiceName), t.Count, cfg.CallbackURL("/services/"+t.ServiceID+"/alerts")),
			false))
	case notification.ScheduleOnCallUsers:
		opts = append(opts, slack.MsgOptionText(s.onCallNotificationText(ctx, t), false))
	default:
		return nil, errors.Errorf("unsupported message type: %T", t)
	}

	var msgTS string
	err := s.withClient(ctx, func(c *slack.Client) error {
		_, _msgTS, err := c.PostMessageContext(ctx, msg.Destination().Value, opts...)
		if err != nil {
			return err
		}
		msgTS = _msgTS
		return nil
	})
	if err != nil {
		return nil, err
	}

	if isUpdate {
		msgTS = ""
	}

	return &notification.SentMessage{
		ExternalID: msgTS,
		State:      notification.StateDelivered,
	}, nil
}

func (s *ChannelSender) lookupTeamIDForToken(ctx context.Context, token string) (string, error) {
	var teamID string

	err := s.withClient(ctx, func(c *slack.Client) error {
		info, err := c.AuthTestContext(ctx)
		if err != nil {
			return err
		}

		teamID = info.TeamID

		return nil
	})

	if err != nil {
		return "", err
	}

	return teamID, nil
}
