import { SignedIn, SignedOut, SignInButton, SignOutButton, useUser } from '@thunderid/react'
import './App.css'

function HomeScreen() {
  const { profile, flattenedProfile } = useUser()

  const name = flattenedProfile?.givenName || profile?.givenName || flattenedProfile?.displayName || 'there'
  const email = flattenedProfile?.email || profile?.email || 'no email on file'

  return (
    <div className="home-card">
      <h1>Welcome, {name}!</h1>
      <p className="email">{email}</p>
      <p className="hint">You're signed in via Demo Partner App's ThunderID integration.</p>
      <SignOutButton>Sign Out</SignOutButton>
    </div>
  )
}

function App() {
  return (
    <section id="center">
      <SignedIn>
        <HomeScreen />
      </SignedIn>
      <SignedOut>
        <div className="landing">
          <h1>Demo Partner App</h1>
          <p>Sign in to access your account.</p>
          <SignInButton>Sign In</SignInButton>
        </div>
      </SignedOut>
    </section>
  )
}

export default App
