# booth-streamlit

Project Booth's code-first dashboard/app module: a user writes a Streamlit app in Python and
runs it as a hosted app, reached through the shell via `iframe-proxy` (ADR 0016). One of three
modules in the "dashboards/apps" capability slot, alongside `booth-superset` and
`booth-metabase`.

Architecture, contracts and decisions live in `booth-architecture`; this repo's brief is
`agent-briefs/streamlit.md` there.
