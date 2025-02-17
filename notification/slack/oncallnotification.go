package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack/slackutilsx"
	"github.com/target/goalert/notification"
	"github.com/target/goalert/user"
	"github.com/target/goalert/util/log"
)

// onCallNotificationText will return text intended to be sent to Slack representing a ScheduleOnCallUsers notification.
//
// It gracefully degrades to excluding slack IDs when there is an error fetching the required information (e.g., team ID or
// auth subjects).
func (s *ChannelSender) onCallNotificationText(ctx context.Context, t notification.ScheduleOnCallUsers) string {
	if len(t.Users) == 0 {
		return renderOnCallNotificationMessage(t, nil)
	}

	teamID, err := s.TeamID(ctx)
	if err != nil {
		log.Log(ctx, fmt.Errorf("lookup team ID: %w", err))
		return renderOnCallNotificationMessage(t, nil)
	}

	userIDs := make([]string, len(t.Users))
	for i, u := range t.Users {
		userIDs[i] = u.ID
	}

	userSlackIDs := make(map[string]string, len(t.Users))
	err = s.cfg.UserStore.AuthSubjectsFunc(ctx, "slack:"+teamID, userIDs, func(sub user.AuthSubject) error {
		userSlackIDs[sub.UserID] = sub.SubjectID
		return nil
	})
	if err != nil {
		log.Log(ctx, fmt.Errorf("lookup auth subjects for slack: %w", err))
		// handled error by logging, continue on to render message with any included slack IDs
	}

	return renderOnCallNotificationMessage(t, userSlackIDs)
}

// renderOnCallNotificationMessage will render a message for Slack including links for the schedule and any users.
//
// If a user's ID is available in userSlackIDs, an `@` user mention will be used in place of a link to the GoAlert user's detail page.
func renderOnCallNotificationMessage(msg notification.ScheduleOnCallUsers, userSlackIDs map[string]string) string {

	var userLinks []string
	for _, u := range msg.Users {
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

	return fmt.Sprintf(`
New On-Call Rotation for this week!
Personnel: %s
Schedule: <%s|%s>
Please ACKNOWLEDGE and CLOSE any triggered alerts ASAP!
		`,
		users,
		slackutilsx.EscapeMessage(msg.ScheduleURL),
		slackutilsx.EscapeMessage(msg.ScheduleName),
	)
}
