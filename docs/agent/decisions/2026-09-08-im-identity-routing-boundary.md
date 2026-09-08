# IM identity routing boundary

**Decision**

Keep P0 grouping display-only. Bind codes, unbinding, and D6 remain scoped to `ConnectionID`.

**Reason**

Connections with the same URL can use different Bot credentials. A Mattermost direct-message channel belongs to a Bot-user pair, so a channel created by one Bot cannot be used by another. URL equality is not sufficient evidence to share identities or routes.

**Cost to reverse**

The UI grouping is reversible. Cross-connection routing must wait for an additive, verified IM-instance model and live RBAC tests; otherwise it can send notifications through an unauthorized Bot or invalid DM channel.
