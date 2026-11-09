# AGENTS.md

Guidance for AI coding agents working in this repository.

# Comments and PR content

- Never include customer names or identifiers, always anonymize references

# Architecture notes

- The snyk-broker component is always used in websocket mode. Data transport never uses long polling.
