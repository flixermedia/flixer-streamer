# GitHub Copilot Instructions — jellyfin-plugin-flixer

## Agent autonomy

You are operating with Autopilot / Allow All permissions.

These permissions authorize routine tool execution, repository inspection, code modifications, testing and implementation work. They do **not** authorize making unresolved product or architectural decisions on behalf of the project owner.

## Mandatory clarification

You MUST ask the project owner and wait for a response when:

- Requirements are ambiguous, incomplete or contradictory.
- Multiple reasonable approaches would produce different user-facing behaviour.
- A decision affects authentication, authorization, subscriptions, payments, entitlements or media access.
- A technical limitation requires changing an agreed project requirement.
- A significant architectural decision cannot be resolved confidently through repository inspection.
- You are unsure whether the project owner would approve a proposed behaviour.

Do not invent requirements or silently assume answers.

When asking a question, briefly explain the available options, their trade-offs and your recommended approach.

## Independent engineering decisions

You SHOULD autonomously:

- Inspect relevant Flixer and Jellyfin repositories.
- Verify existing APIs, services and architectural patterns.
- Research Jellyfin plugin compatibility.
- Reuse existing Flixer capabilities wherever appropriate.
- Implement routine technical details.
- Write and run tests.
- Challenge incorrect assumptions in the project README.
- Propose safer or more maintainable approaches.

The README describes project goals and current decisions, not an inflexible implementation specification.

## Stop-and-wait rule

If a decision requires clarification, stop the affected implementation and wait for the project owner's answer.

Do not interpret Autopilot / Allow All as permission to bypass this rule.
