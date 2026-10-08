from booth_streamlit import User, user_from_headers


def test_reads_the_verified_headers_case_insensitively():
    got = user_from_headers({"x-booth-user": "3f1c", "X-Booth-Workspace": "acme", "X-BOOTH-ROLE": "editor"})
    assert got == User(subject="3f1c", workspace="acme", role="editor")


def test_none_outside_booth_or_on_incomplete_headers():
    assert user_from_headers({}) is None
    assert user_from_headers({"X-Booth-User": "u", "X-Booth-Workspace": "acme"}) is None
    assert user_from_headers({"X-Booth-User": "u", "X-Booth-Workspace": "acme", "X-Booth-Role": "admin"}) is None
