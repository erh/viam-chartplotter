import 'package:flutter/material.dart';

import '../auth/viam_session.dart';

/// The sign-in landing screen. A single "Sign in with Viam" button kicks off
/// the system-browser OAuth flow on [ViamSession].
class LoginScreen extends StatelessWidget {
  const LoginScreen({super.key, required this.session});
  final ViamSession session;

  @override
  Widget build(BuildContext context) {
    final signingIn = session.status == AuthStatus.signingIn;
    return Scaffold(
      body: Center(
        child: Padding(
          padding: const EdgeInsets.all(32),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              const Icon(Icons.sailing, size: 72),
              const SizedBox(height: 16),
              Text('Viam Chartplotter',
                  style: Theme.of(context).textTheme.headlineSmall),
              const SizedBox(height: 32),
              // A stored session that only failed to come up because Viam was
              // unreachable is worth one tap, not a whole browser round trip —
              // so it leads, and the full sign-in becomes the fallback.
              if (session.canRetryRestore) ...[
                FilledButton.icon(
                  onPressed: signingIn ? null : session.retryRestore,
                  icon: signingIn
                      ? const SizedBox(
                          width: 18,
                          height: 18,
                          child: CircularProgressIndicator(strokeWidth: 2),
                        )
                      : const Icon(Icons.refresh),
                  label: Text(signingIn ? 'Reconnecting…' : 'Resume session'),
                ),
                const SizedBox(height: 12),
                TextButton.icon(
                  onPressed: signingIn ? null : session.signIn,
                  icon: const Icon(Icons.login),
                  label: const Text('Sign in again instead'),
                ),
              ] else
                FilledButton.icon(
                  onPressed: signingIn ? null : session.signIn,
                  icon: signingIn
                      ? const SizedBox(
                          width: 18,
                          height: 18,
                          child: CircularProgressIndicator(strokeWidth: 2),
                        )
                      : const Icon(Icons.login),
                  label: Text(signingIn ? 'Signing in…' : 'Sign in with Viam'),
                ),
              if (session.error != null) ...[
                const SizedBox(height: 16),
                // Selectable so the full error can be copied out of the app
                // (plain Text can't be selected).
                SelectableText(
                  session.error!,
                  textAlign: TextAlign.center,
                  style: TextStyle(color: Theme.of(context).colorScheme.error),
                ),
              ],
            ],
          ),
        ),
      ),
    );
  }
}
