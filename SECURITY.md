# Politique de sécurité & confidentialité — GMINI-Web-Proxy

## Principes de sécurité par défaut

GMINI-Web-Proxy est conçu pour un usage **strictement local** :

- **Écoute locale** : par défaut sur `127.0.0.1:8081` uniquement (refuse les ports exposés sans configuration explicite).
- **Authentification obligatoire** : le serveur refuse de démarrer sans `GEMINI_API_KEYS` (sauf opt-in explicite `GEMINI_ALLOW_NO_AUTH=1` réservé aux tests locaux).
- **Permissions restrictives** :
  - Binaires compilés : `0700` (exécutable uniquement par votre utilisateur).
  - Fichiers de configuration, cookies, état et journaux : `0600` (lecture/écriture uniquement par votre utilisateur).
- **Anti-SSRF** : le téléchargement d'images distantes (`GEMINI_ALLOW_REMOTE_IMAGES`) est désactivé par défaut.

---

## Fichiers sensibles à ne JAMAIS partager

Ne publiez, ne commitez et ne collez **jamais** dans une issue, un forum ou un chat public :

| Fichier / Donnée | Contenu sensible |
|---|---|
| `cookie.txt` | Cookies de session Google (`__Secure-1PSID`, `__Secure-1PSIDTS`, `SAPISID`) |
| `.env` | Clés API locales (`GEMINI_API_KEYS`) et chemins locaux |
| `*.state.json` | Jeton XSRF Google (`at`), état interne de session |
| `custom_models.json` | Configuration locale des modèles Antigravity |
| `traces/*.json` | Contient les prompts, pensées et réponses complètes |
| URL `/sorry` | Paramètres de défi anti-robot liés à votre session |

Le fichier [`.gitignore`](.gitignore) exclut automatiquement `.env`, `*.txt`, `*.state.json`, `*.calls.jsonl`, `*.tokens.jsonl`, `*.flights.json`, `*.bak.*` et `.DS_Store`.

---

## Signaler une vulnérabilité

Si vous identifiez une faille de sécurité (fuite de secret dans les logs, contournement d'authentification locale, SSRF, etc.) :

1. **N'ouvrez pas d'issue publique contenant de données sensibles ou un exploit détaillé.**
2. Ouvrez une issue de sécurité privée (*GitHub Security Advisory* si activé sur le dépôt) ou contactez le mainteneur via le dépôt GitHub en décrivant le vecteur sans inclure de secret réel.
3. Pour joindre des éléments techniques de diagnostic, utilisez exclusivement l'export anonymisé (`/_ui` → **Diagnostic Proxy** → **Exporter** ou `python3 tools/export_gmini_diagnostics.py`).
