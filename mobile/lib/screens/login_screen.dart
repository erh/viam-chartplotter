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
                Text(
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
