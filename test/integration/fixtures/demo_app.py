import streamlit as st
import booth_streamlit

st.title("booth-streamlit demo app")
u = booth_streamlit.user()
st.write(f"marker:user={u.subject if u else 'none'}")
st.write(f"marker:workspace={u.workspace if u else 'none'}")
st.write(f"marker:role={u.role if u else 'none'}")
headers = {k.lower() for k in st.context.headers.keys()}
st.write(f"marker:identity-header-visible={'x-booth-identity' in headers}")
st.write(f"marker:gate-token-visible={'x-booth-gate-token' in headers}")
st.write(f"marker:core-cookie-visible={'booth_iframe_session' in st.context.cookies}")
if st.button("Rerun"):
    st.write("marker:button-roundtrip=ok")
