package cron

// MarkerNotificationPolicy is the instruction appended to a when_relevant job
// whose result is judged by the agent's own [SILENT] marker.
//
// The desktop uses it because there is no judgment gate there — the reply is
// already in the conversation, so the marker only suppresses the notification.
// The channel uses it when notify_matcher resolves to the marker (the gate's
// own wording lives with the gate, in channel/manager).
const MarkerNotificationPolicy = "[Notification policy: when_relevant]\n" +
	"After completing the task, evaluate whether the result contains meaningful or actionable information for the user. " +
	"If there is nothing new, no changes detected, or nothing worth reporting, respond with ONLY the text `[SILENT]` — " +
	"do not include any other content. The notification will be suppressed and the user will not be disturbed. " +
	"Only send actual content when there IS something noteworthy to report."
