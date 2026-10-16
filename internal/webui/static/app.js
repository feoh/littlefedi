		// htmx settles a swap by copying class and style off the element it
		// replaces onto the replacement with the same id, then putting the
		// served values back 20ms later. Alpine initializes in between, so the
		// restore drops the display:none x-show had just written and nothing
		// writes it back again. The composer's preview panel and suggestion
		// list carry ids for aria-controls, so both return open and empty after
		// a post. No rule here animates during the settle.
		if (window.htmx) htmx.config.attributesToSettle = [];

		(function() {
			const key = 'littlefedi.landing-page.v1';

			function landingPage() {
				if (!document.documentElement.hasAttribute('data-catchup-enabled')) return '/';
				try { return localStorage.getItem(key) === '/catchup' ? '/catchup' : '/'; } catch (_) { return '/'; }
			}

			const initial = landingPage();
			if (initial !== '/' && location.pathname === '/' && !location.search && !location.hash) {
				location.replace(initial);
				return;
			}

			function apply() {
				const selected = landingPage();
				document.querySelectorAll('.logo').forEach(function(link) { link.href = selected; });
				const picker = document.getElementById('landing-page');
				if (picker) picker.value = selected;
			}

			document.addEventListener('change', function(event) {
				if (event.target.id !== 'landing-page') return;
				try { localStorage.setItem(key, event.target.value); } catch (_) {}
				apply();
			});
			document.addEventListener('DOMContentLoaded', apply);
			document.addEventListener('htmx:afterSettle', apply);
		})();

		document.addEventListener('alpine:init', function() {
			Alpine.data('composerState', function() {
	            return {
                // initialText (edit/redraft prefill) wins; otherwise seed a reply
                // with the mention prefix. Kept in sync with the server-rendered
                // textarea content so the composer also works without JavaScript.
	                text: '',
	                mode: 'plain',
	                previousMode: 'plain',
	                markdownEnabled: false,
	                fullscreen: false,
	                expanded: true,
	                visualSupported: false,
	                visualHTML: '',
	                visualText: '',
	                blockStyle: 'p',
	                linkURL: '',
	                linking: false,
	                previewLoading: false,
	                previewSourceSynced: false,
	                previewSensitiveRevealed: false,
	                editorError: '',
	                spoilerText: '',
	                sensitive: false,
	                pollOptions: ['', '', '', ''],
	                pollMultiple: false,
	                mediaPreviews: {},
	                dropping: false,
	                _dragDepth: 0,
	                _pageDrop: null,
	                toolbarTick: 0,
	                _savedRange: null,
	                _triggerRange: null,
	                _selectionListener: null,
	                _renderController: null,
	                visibility: 'public',
	                isBlogPost: false,
	                mediaRows: 1,
	                maxMedia: 4,
	                mediaThumbnails: {},
	                mediaPosterJobs: {},
	                emojiGroup: '',
	                emojiRecents: [],
	                _emojiBuilt: false,
	                _emojiEntries: null,
	                _emojiLabels: null,
	                _emojiMarkup: {},
	                _emojiSearchTimer: null,
	                _emojiSearchController: null,
	                _emojiSearchId: 0,

	                init() {
					const data = this.$root.dataset;
					const initialText = data.initialText || '';
					const initialHTML = data.initialHtml || '';
					const replyMention = data.replyMention || '';
					this.text = initialText || (replyMention ? replyMention + ' ' : '');
					this.markdownEnabled = !!data.initialMarkdown;
					this.expanded = !!data.startExpanded;
					this.visualSupported = 'contentEditable' in document.documentElement && typeof document.execCommand === 'function';
					let preferred = this.markdownEnabled ? 'markdown' : 'plain';
					try {
						const saved = localStorage.getItem('littlefedi.composer.mode.v1');
						if (data.draftFormatSet && this.markdownEnabled && saved === 'wysiwyg') preferred = saved;
					} catch (_) {}
					if (!this.visualSupported && preferred === 'wysiwyg') preferred = 'markdown';
					if (data.draftFormatSet && !this.markdownEnabled) preferred = 'plain';
					this.mode = preferred;
					this.previousMode = preferred;
					this.markdownEnabled = preferred !== 'plain';
					this.visibility = data.defaultVisibility || 'public';
					this.isBlogPost = !!data.isBlogPost;
					this.maxMedia = Number(data.maxMedia) || 4;
					this.spoilerText = this.$root.elements.spoiler_text ? this.$root.elements.spoiler_text.value : '';
					this.sensitive = !!(this.$root.elements.sensitive && this.$root.elements.sensitive.checked);
					this._selectionListener = () => this.saveSelection();
					document.addEventListener('selectionchange', this._selectionListener);
					// A file that misses the composer by a few pixels would make
					// the browser open it in place of the page, losing the draft.
					// The main composer takes those drops instead.
					if (this.$root.parentElement && this.$root.parentElement.id === 'composer') {
						this._pageDrop = event => {
							if (event.defaultPrevented || !this.draggingFiles(event)) return;
							event.preventDefault();
							if (event.type === 'drop') this.addFiles(event.dataTransfer.files);
						};
						window.addEventListener('dragover', this._pageDrop);
						window.addEventListener('drop', this._pageDrop);
					}
	                    // Lets the server convert the composer's timezone-less
                    // datetime-local values (schedule, expiry) to UTC correctly;
                    // absent without JS, in which case the server falls back to
                    // treating them as its own local time. The page-wide pass at
                    // the bottom of this file covers forms outside the composer.
                    fillTimezoneOffsets(this.$el);
					this.$root.addEventListener('formdata', event => {
						Object.entries(this.mediaThumbnails).forEach(([index, blob]) => {
							event.formData.append('media_thumbnail_' + index, blob, 'poster.jpg');
						});
					});
					if (this.mode === 'wysiwyg') this.$nextTick(() => {
						if (initialHTML) {
							this.visualHTML = initialHTML;
							this.$refs.visual.innerHTML = initialHTML;
							this.visualText = this.$refs.visual.innerText;
						} else {
							this.loadVisual(this.markdownEnabled ? 'markdown' : 'plain');
						}
					});
                },

				destroy() {
					if (this._selectionListener) document.removeEventListener('selectionchange', this._selectionListener);
					if (this._pageDrop) {
						window.removeEventListener('dragover', this._pageDrop);
						window.removeEventListener('drop', this._pageDrop);
					}
					if (this._renderController) this._renderController.abort();
					document.documentElement.classList.remove('composer-fullscreen-open');
					Object.values(this.mediaPreviews).forEach(media => URL.revokeObjectURL(media.url));
				},

				setFullscreen(enabled) {
					this.fullscreen = !!enabled;
					document.documentElement.classList.toggle('composer-fullscreen-open', this.fullscreen);
				},

				toggleFullscreen() { this.setFullscreen(!this.fullscreen); },

	                selectMedia(index, event) {
                    const file = event.target.files[0];
                    if (event.target.files.length && this.mediaRows < this.maxMedia) {
                        this.mediaRows = Math.max(this.mediaRows, index + 2);
                    }
					delete this.mediaThumbnails[index];
					delete this.mediaPosterJobs[index];
					if (this.mediaPreviews[index]) URL.revokeObjectURL(this.mediaPreviews[index].url);
					const previews = Object.assign({}, this.mediaPreviews);
					if (!file) {
						delete previews[index];
						this.mediaPreviews = previews;
						return;
					}
					let type = file.type.split('/')[0];
					if (!['image', 'video', 'audio'].includes(type)) type = /\.(mp4|m4v|mov|webm|ogv)$/i.test(file.name) ? 'video' : (/\.(mp3|m4a|ogg|wav|flac)$/i.test(file.name) ? 'audio' : 'image');
					previews[index] = {index: index, type: type, url: URL.createObjectURL(file)};
					this.mediaPreviews = previews;
					if (!file.type.startsWith('video/') && !/\.(mp4|m4v|mov|webm|ogv)$/i.test(file.name)) return;
					this.mediaPosterJobs[index] = this.createVideoPoster(file).then(blob => {
						if (blob && this.$root.elements['media_' + index].files[0] === file) this.mediaThumbnails[index] = blob;
					}).finally(() => {
						if (this.$root.elements['media_' + index].files[0] === file) delete this.mediaPosterJobs[index];
					});
	                },

				clearMedia(index) {
					const input = this.$root.elements['media_' + index];
					if (input) input.value = '';
					const description = this.$root.elements['media_description_' + index];
					if (description) description.value = '';
					delete this.mediaThumbnails[index];
					delete this.mediaPosterJobs[index];
					if (this.mediaPreviews[index]) URL.revokeObjectURL(this.mediaPreviews[index].url);
					const previews = Object.assign({}, this.mediaPreviews);
					delete previews[index];
					this.mediaPreviews = previews;
				},

				// Dropped and pasted files go into the same file inputs the
				// picker uses, so the form posts exactly what it would post
				// without JavaScript and the server needs no other path.
				addFiles(files) {
					files = Array.from(files).filter(file => /^(image|video|audio)\//.test(file.type));
					if (!files.length) return;
					const kept = Array.from(this.$root.querySelectorAll('input[name="media_ids"]')).filter(box => box.checked).length;
					let room = this.maxMedia - kept;
					for (let index = 0; index < this.maxMedia && files.length && room > 0; index++) {
						const input = this.$root.elements['media_' + index];
						if (!input) break;
						if (input.files.length) { room--; continue; }
						try {
							const transfer = new DataTransfer();
							transfer.items.add(files.shift());
							input.files = transfer.files;
						} catch (_) {
							return;
						}
						this.selectMedia(index, {target: input});
						room--;
					}
					this.expanded = true;
					const row = this.$root.querySelector('.composer-media-row');
					if (row) row.closest('details').open = true;
					if (files.length) this.editorError = this.$root.dataset.mediaLimit;
				},

				pasteFiles(event) {
					if (!event.clipboardData || !event.clipboardData.files.length) return;
					event.preventDefault();
					this.addFiles(event.clipboardData.files);
				},

				draggingFiles(event) {
					return !!event.dataTransfer && Array.from(event.dataTransfer.types).includes('Files');
				},

				dragEnter(event) {
					if (!this.draggingFiles(event)) return;
					event.preventDefault();
					this._dragDepth++;
					this.dropping = true;
				},

				dragOver(event) {
					if (this.draggingFiles(event)) event.preventDefault();
				},

				dragLeave(event) {
					if (!this.draggingFiles(event)) return;
					this._dragDepth = Math.max(0, this._dragDepth - 1);
					if (!this._dragDepth) this.dropping = false;
				},

				dropFiles(event) {
					if (!this.draggingFiles(event)) return;
					event.preventDefault();
					this._dragDepth = 0;
					this.dropping = false;
					this.addFiles(event.dataTransfer.files);
				},

				prepareComposerSubmit(event) {
					this.editorError = '';
					if (this.editableMode() === 'wysiwyg') {
						const html = this.$refs.visual ? this.$refs.visual.innerHTML : this.visualHTML;
						if (new Blob([html]).size > 1048576) {
							event.preventDefault();
							this.editorError = this.$root.dataset.editorTooLarge || 'Visual post is too large';
							return;
						}
						this.$refs.wysiwygInput.value = html;
						this.markdownEnabled = true;
					}
					const jobs = Object.values(this.mediaPosterJobs);
					if (!jobs.length) return;
					event.preventDefault();
					const submitter = event.submitter;
					Promise.allSettled(jobs).then(() => this.$root.requestSubmit(submitter));
				},

				editableMode() {
					return this.mode === 'preview' ? this.previousMode : this.mode;
				},

				rememberMode(mode) {
					try { localStorage.setItem('littlefedi.composer.mode.v1', mode); } catch (_) {}
				},

				async setMode(next) {
					if (next === this.mode || (next === 'wysiwyg' && !this.visualSupported)) return;
					this.closeSuggestions();
					this.editorError = '';
					const current = this.editableMode();
					if (next === 'preview') {
						this.previousMode = current;
						this.mode = 'preview';
						await this.renderPreview();
						return;
					}
					if (current === 'wysiwyg' && next !== 'wysiwyg' && !(this.mode === 'preview' && this.previewSourceSynced)) {
						try {
							const result = await this.renderRequest('wysiwyg');
							this.text = result.source;
						} catch (error) {
							if (error.name === 'AbortError') return;
							this.editorError = this.$root.dataset.previewError || 'Unable to convert the post';
							return;
						}
					}
					if (next === 'wysiwyg' && current !== 'wysiwyg') {
						if (!await this.loadVisual(current)) return;
					}
					this.mode = next;
					this.previousMode = next;
					this.markdownEnabled = next !== 'plain';
					this.rememberMode(next);
				},

				async moveMode(delta) {
					const modes = ['plain', 'markdown'].concat(this.visualSupported ? ['wysiwyg'] : [], ['preview']);
					const index = modes.indexOf(this.mode);
					await this.focusMode(modes[(index + delta + modes.length) % modes.length]);
				},

				async focusMode(mode) {
					await this.setMode(mode);
					this.$nextTick(() => {
						const tab = this.$root.querySelector('[data-editor-mode="' + mode + '"]');
						if (tab) tab.focus();
					});
				},

				async loadVisual(format) {
					try {
						const result = await this.renderRequest(format === 'plain' ? 'plain' : 'markdown');
						this.visualHTML = result.html;
						this.visualText = this.text;
						this.$nextTick(() => {
							if (this.$refs.visual) {
								this._savedRange = null;
								this.$refs.visual.innerHTML = result.html;
								this.visualText = this.$refs.visual.innerText;
							}
						});
						return true;
					} catch (error) {
						if (error.name === 'AbortError') return false;
						this.editorError = this.$root.dataset.previewError || 'Unable to open the visual editor';
						return false;
					}
				},

				async renderPreview() {
					this.previewLoading = true;
					this.previewSourceSynced = false;
					this.previewSensitiveRevealed = false;
					try {
						const result = await this.renderRequest(this.previousMode);
						if (this.previousMode === 'wysiwyg') {
							this.text = result.source;
							this.previewSourceSynced = true;
						}
						this.$refs.preview.innerHTML = result.html;
					} catch (error) {
						if (error.name === 'AbortError') return;
						this.editorError = this.$root.dataset.previewError || 'Unable to render preview';
					} finally {
						this.previewLoading = false;
					}
				},

				async renderRequest(format) {
					if (this._renderController) this._renderController.abort();
					const controller = new AbortController();
					this._renderController = controller;
					const body = new URLSearchParams({format: format});
					if (format === 'wysiwyg') body.set('wysiwyg_html', this.$refs.visual ? this.$refs.visual.innerHTML : this.visualHTML);
					else body.set('source', this.text);
					const csrf = this.$root.elements.csrf_token ? this.$root.elements.csrf_token.value : '';
					const response = await fetch('/partial/composer/preview', {
						method: 'POST', credentials: 'same-origin', signal: controller.signal,
						headers: {'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf}, body: body
					});
					if (!response.ok) throw new Error('preview ' + response.status);
					return response.json();
				},

				selectedMedia() { return Object.values(this.mediaPreviews).sort((a, b) => a.index - b.index); },
				mediaDescription(index) {
					const field = this.$root.elements['media_description_' + index];
					return field ? field.value : '';
				},
				keptMedia(id) {
					return Array.from(this.$root.querySelectorAll('input[name="media_ids"]')).some(input => input.value === id && input.checked);
				},
				mediaCount() {
					return this.selectedMedia().length + Array.from(this.$root.querySelectorAll('input[name="media_ids"]:checked')).length;
				},
				nonEmptyPollOptions() { return this.pollOptions.map(value => value.trim()).filter(Boolean); },

				editorLength() {
					return this.editableMode() === 'wysiwyg' ? this.visualText.length : this.text.length;
				},

				createVideoPoster(file) {
					return new Promise(resolve => {
						const video = document.createElement('video');
						const url = URL.createObjectURL(file);
						let timer;
						let finished = false;
						const done = blob => {
							if (finished) return;
							finished = true;
							clearTimeout(timer);
							URL.revokeObjectURL(url);
							resolve(blob);
						};
						video.muted = true;
						video.preload = 'auto';
						video.onloadeddata = () => {
							const scale = Math.min(1, 640 / video.videoWidth);
							const canvas = document.createElement('canvas');
							canvas.width = Math.max(1, Math.round(video.videoWidth * scale));
							canvas.height = Math.max(1, Math.round(video.videoHeight * scale));
							canvas.getContext('2d').drawImage(video, 0, 0, canvas.width, canvas.height);
							canvas.toBlob(done, 'image/jpeg', 0.82);
						};
						video.onerror = () => done(null);
						timer = setTimeout(() => done(null), 10000);
						video.src = url;
					});
				},

				hideBrokenImage(event) {
					if (event && event.target) event.target.style.display = 'none';
				},

                // Autocomplete state
                showSuggestions: false,
                suggestions: [],
                selectedIndex: -1,
                triggerMode: '',     // 'mention' | 'tag' | 'emoji'
                triggerStart: -1,
				triggerEnd: -1,
                triggerQuery: '',
                _fetchId: 0,
                _fetchTimer: null,
                _fetchController: null,

                // Called on input to detect @mentions and #hashtags
                onInput() {
                    this.emojifyTyped();
                    this.detectTrigger();
                },

				onVisualInput() {
					if (!this.$refs.visual) return;
					this.previewSourceSynced = false;
					this.visualHTML = this.$refs.visual.innerHTML;
					this.visualText = this.$refs.visual.innerText;
					this.emojifyTyped();
					this.detectTrigger();
					this.saveSelection();
				},

				selectionInVisual() {
					const selection = window.getSelection();
					const visual = this.$refs.visual;
					return !!(selection && selection.rangeCount && visual && visual.contains(selection.anchorNode) && visual.contains(selection.focusNode));
				},

				selectionBlockStyle(node) {
					const visual = this.$refs.visual;
					let element = node && (node.nodeType === Node.ELEMENT_NODE ? node : node.parentElement);
					while (element && element !== visual) {
						const tag = element.tagName.toLowerCase();
						if (['p', 'h1', 'h2', 'h3', 'blockquote', 'pre'].includes(tag)) return tag;
						element = element.parentElement;
					}
					return 'p';
				},

				saveSelection() {
					if (!this.selectionInVisual()) return false;
					const selection = window.getSelection();
					this._savedRange = selection.getRangeAt(0).cloneRange();
					this.blockStyle = this.selectionBlockStyle(selection.anchorNode);
					this.toolbarTick++;
					return true;
				},

				restoreSelection() {
					const visual = this.$refs.visual;
					if (!this._savedRange || !visual || !this._savedRange.startContainer.isConnected || !visual.contains(this._savedRange.commonAncestorContainer)) {
						this._savedRange = null;
						return false;
					}
					const selection = window.getSelection();
					selection.removeAllRanges();
					selection.addRange(this._savedRange);
					return true;
				},

				activateVisualSelection() {
					const visual = this.$refs.visual;
					if (!visual) return false;
					if (document.activeElement === visual && this.selectionInVisual()) return true;
					visual.focus({preventScroll: true});
					return this.restoreSelection() || this.selectionInVisual();
				},

				command(name, value) {
					if (!this.activateVisualSelection()) return false;
					let applied = false;
					try { applied = document.execCommand(name, false, value || null); } catch (_) {}
					if (!applied) {
						if (!['undo', 'redo', 'unlink'].includes(name)) this.editorError = this.$root.dataset.editorCommandError || 'Unable to format the selection';
						return false;
					}
					this.editorError = '';
					this.onVisualInput();
					return true;
				},

				selectionCode(range) {
					const visual = this.$refs.visual;
					const closest = node => {
						const element = node && (node.nodeType === Node.ELEMENT_NODE ? node : node.parentElement);
						const code = element && element.closest('code');
						return code && visual.contains(code) ? code : null;
					};
					if (range.collapsed) return closest(range.startContainer);
					const boundary = (node, offset, end) => node.nodeType === Node.ELEMENT_NODE ? (node.childNodes[offset - (end ? 1 : 0)] || node) : node;
					const start = closest(boundary(range.startContainer, range.startOffset, false));
					return start && start === closest(boundary(range.endContainer, range.endOffset, true)) ? start : null;
				},

				commandState(name) {
					this.toolbarTick;
					if (!this.selectionInVisual()) return false;
					if (name === 'code') return !!this.selectionCode(window.getSelection().getRangeAt(0));
					try { return document.queryCommandState(name); } catch (_) { return false; }
				},

				formatBlock(tag) {
					if (['p', 'h1', 'h2', 'h3', 'blockquote', 'pre'].includes(tag)) this.command('formatBlock', tag);
				},

				inlineCode() {
					if (!this.activateVisualSelection()) return;
					const selection = window.getSelection();
					if (!selection || !selection.rangeCount) return;
					const range = selection.getRangeAt(0);
					const existing = this.selectionCode(range);
					const unwrap = code => {
						while (code.firstChild) code.parentNode.insertBefore(code.firstChild, code);
						code.remove();
					};
					if (existing) {
						const first = existing.firstChild;
						const last = existing.lastChild;
						unwrap(existing);
						if (!range.collapsed && first && last) {
							range.setStartBefore(first);
							range.setEndAfter(last);
						}
					} else if (range.collapsed) {
						const code = document.createElement('code');
						const text = code.appendChild(document.createTextNode(''));
						range.insertNode(code);
						range.setStart(text, 0);
						range.collapse(true);
					} else {
						const code = document.createElement('code');
						const contents = range.extractContents();
						contents.querySelectorAll('code').forEach(unwrap);
						code.appendChild(contents);
						range.insertNode(code);
						range.selectNodeContents(code);
					}
					selection.removeAllRanges();
					selection.addRange(range);
					this.onVisualInput();
				},

				createLink() {
					if (!this.activateVisualSelection()) return;
					this.saveSelection();
					this.linkURL = 'https://';
					this.linking = true;
					this.$nextTick(() => {
						this.$refs.linkInput.focus();
						this.$refs.linkInput.select();
					});
				},

				applyLink() {
					const value = this.linkURL.trim();
					if (!value) { this.cancelLink(); return; }
					try {
						const url = new URL(value);
						if (url.protocol !== 'http:' && url.protocol !== 'https:') return;
					} catch (_) { return; }
					this.linking = false;
					this.command('createLink', value);
				},

				cancelLink() {
					this.linking = false;
					this.$nextTick(() => this.activateVisualSelection());
				},

				pasteVisual(event) {
					if (event.clipboardData.files.length) return;
					event.preventDefault();
					const html = event.clipboardData.getData('text/html');
					if (html) document.execCommand('insertHTML', false, this.cleanPastedHTML(html));
					else document.execCommand('insertText', false, event.clipboardData.getData('text/plain'));
					this.onVisualInput();
				},

				cleanPastedHTML(source) {
					const parsed = new DOMParser().parseFromString(source, 'text/html');
					const allowed = new Set(['P', 'BR', 'STRONG', 'B', 'EM', 'I', 'DEL', 'S', 'BLOCKQUOTE', 'UL', 'OL', 'LI', 'CODE', 'PRE', 'H1', 'H2', 'H3', 'HR', 'A']);
					const clean = node => {
						if (node.nodeType === Node.TEXT_NODE) return document.createTextNode(node.data);
						const fragment = document.createDocumentFragment();
						if (node.nodeType !== Node.ELEMENT_NODE || ['SCRIPT', 'STYLE', 'TEMPLATE', 'IMG', 'VIDEO', 'AUDIO'].includes(node.tagName)) return fragment;
						const target = allowed.has(node.tagName) ? document.createElement(node.tagName.toLowerCase()) : fragment;
						if (node.tagName === 'A' && /^https?:\/\//i.test(node.getAttribute('href') || '')) target.href = node.getAttribute('href');
						Array.from(node.childNodes).forEach(child => target.appendChild(clean(child)));
						return target;
					};
					const holder = document.createElement('div');
					Array.from(parsed.body.childNodes).forEach(node => holder.appendChild(clean(node)));
					return holder.innerHTML;
				},

				editorElement() { return this.editableMode() === 'wysiwyg' ? this.$refs.visual : this.$refs.textarea; },
				editorText() { return this.editableMode() === 'wysiwyg' ? this.visualText : this.text; },

				caretOffset() {
					const el = this.editorElement();
					if (!el) return 0;
					if (el === this.$refs.textarea) return el.selectionStart;
					const selection = window.getSelection();
					if (!selection || !selection.rangeCount || !el.contains(selection.anchorNode)) return this.visualText.length;
					const range = selection.getRangeAt(0).cloneRange();
					range.selectNodeContents(el);
					range.setEnd(selection.anchorNode, selection.anchorOffset);
					return range.toString().length;
				},

				replaceEditorText(start, end, insert) {
					const el = this.editorElement();
					if (el === this.$refs.textarea) {
						this.text = this.text.slice(0, start) + insert + this.text.slice(end);
						this.$nextTick(() => {
							el.setSelectionRange(start + insert.length, start + insert.length);
							el.focus();
						});
						return;
					}
					const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
					let node, offset = 0, startNode, startOffset, endNode, endOffset;
					while ((node = walker.nextNode())) {
						const next = offset + node.data.length;
						if (!startNode && start <= next) { startNode = node; startOffset = Math.max(0, start - offset); }
						if (end <= next) { endNode = node; endOffset = Math.max(0, end - offset); break; }
						offset = next;
					}
					if (!startNode) {
						startNode = endNode = el.appendChild(document.createTextNode(''));
						startOffset = endOffset = 0;
					}
					if (!endNode) { endNode = startNode; endOffset = startOffset; }
					const range = document.createRange();
					range.setStart(startNode, startOffset);
					range.setEnd(endNode, endOffset);
					range.deleteContents();
					const inserted = document.createTextNode(insert);
					range.insertNode(inserted);
					range.setStartAfter(inserted);
					range.collapse(true);
					const selection = window.getSelection();
					selection.removeAllRanges();
					selection.addRange(range);
					this.visualHTML = el.innerHTML;
					this.visualText = el.innerText;
					el.focus();
					this.saveSelection();
				},

                emojifyTyped() {
                    const m = window.UNICODE_EMOJI;
                    if (!m) return;
					if (this.editableMode() === 'wysiwyg') {
						const selection = window.getSelection();
						if (!selection || !selection.rangeCount || selection.anchorNode.nodeType !== Node.TEXT_NODE) return;
						const node = selection.anchorNode;
						const cursor = selection.anchorOffset;
						const before = node.data.slice(0, cursor);
						const match = before.match(/:([a-zA-Z0-9_]+):$/);
						if (!match) return;
						const char = m.get(match[1]);
						if (!char) return;
						const range = document.createRange();
						range.setStart(node, cursor - match[0].length);
						range.setEnd(node, cursor);
						this.replaceVisualRange(range, char);
						return;
					}
					const el = this.editorElement();
					if (!el) return;
					const value = this.editorText();
					const cursor = this.caretOffset();
					if (cursor < 3) return;
					if (value[cursor - 1] !== ':') return;
					let i = cursor - 2;
					while (i >= 0 && /[a-zA-Z0-9_]/.test(value[i])) i--;
					if (i < 0 || value[i] !== ':') return;
					const shortcode = value.slice(i + 1, cursor - 1);
					const char = m.get(shortcode);
					if (!char) return;
					this.replaceEditorText(i, cursor, char);
                },

                detectTrigger() {
					const el = this.editorElement();
					if (!el) return;
					if (this.editableMode() === 'wysiwyg') {
						const selection = window.getSelection();
						if (!selection || !selection.rangeCount || selection.anchorNode.nodeType !== Node.TEXT_NODE) { this.closeSuggestions(); return; }
						const node = selection.anchorNode;
						const cursor = selection.anchorOffset;
						let start = cursor - 1;
						while (start >= 0 && !/\s/.test(node.data[start])) start--;
						start++;
						const trigger = node.data[start];
						const mode = trigger === '@' ? 'mention' : (trigger === '#' ? 'tag' : (trigger === ':' ? 'emoji' : ''));
						const query = node.data.slice(start + 1, cursor);
						if (!mode || query.length < (mode === 'emoji' ? 1 : 2)) { this.closeSuggestions(); return; }
						if (this.triggerQuery !== query || this.triggerMode !== mode) {
							this.triggerMode = mode;
							this.triggerQuery = query;
							this._triggerRange = document.createRange();
							this._triggerRange.setStart(node, start);
							this._triggerRange.setEnd(node, cursor);
							this.fetchSuggestions(mode, query);
						}
						return;
					}
					const text = this.editorText();
					const cursorPos = this.caretOffset();
                    // Walk backwards from cursor to find start of current word
                    let i = cursorPos - 1;
                    while (i >= 0 && text[i] !== ' ' && text[i] !== '\n' && text[i] !== '\t') {
                        i--;
                    }
                    const wordStart = i + 1;
                    const trigger = text[wordStart];
                    const mode = trigger === '@' ? 'mention' : (trigger === '#' ? 'tag' : (trigger === ':' ? 'emoji' : ''));

                    if (mode && cursorPos > wordStart + 1) {
                        const query = text.slice(wordStart + 1, cursorPos);
                        const minLength = mode === 'emoji' ? 1 : 2;
                        if (query.length >= minLength && (this.triggerQuery !== query || this.triggerMode !== mode)) {
                            this.triggerMode = mode;
							this.triggerStart = wordStart;
							this.triggerEnd = cursorPos;
                            this.triggerQuery = query;
                            this.fetchSuggestions(mode, query);
                        } else if (query.length < minLength) {
                            this.closeSuggestions();
                        }
                    } else {
                        // Just typed the trigger char, or not in a trigger word
                        this.closeSuggestions();
                    }
                },

                // The list opens above the caret so it does not bury the
                // composer buttons, but on the home feed the composer sits at
                // the top of the page and there is barely a line of room up
                // there. Measure first and drop it below when that side is
                // roomier, then cap it to whatever is actually free so the
                // first rows are never cut off the top of the window.
                placeSuggestions() {
                    const list = this.$refs.suggestions;
                    const editor = this.editorElement();
                    if (!list || !editor) return;
                    const box = editor.getBoundingClientRect();
                    const gap = 6;
                    const above = box.top - gap;
                    const below = window.innerHeight - box.bottom - gap;
                    const drop = below > above;
                    list.classList.toggle('composer-autocomplete-below', drop);
                    list.style.maxHeight = Math.max(drop ? below : above, 120) + 'px';
                },

                closeSuggestions() {
                    clearTimeout(this._fetchTimer);
                    this._fetchTimer = null;
                    if (this._fetchController) this._fetchController.abort();
                    this._fetchController = null;
                    this._fetchId++;
                    this.showSuggestions = false;
                    this.suggestions = [];
                    this.selectedIndex = -1;
                    this.triggerQuery = '';
                },

                fetchSuggestions(mode, query) {
                    clearTimeout(this._fetchTimer);
                    if (this._fetchController) this._fetchController.abort();
                    const id = ++this._fetchId;
                    let endpoint;
                    if (mode === 'tag') {
                        endpoint = '/api/tags/autocomplete?q=';
                    } else if (mode === 'emoji') {
                        endpoint = '/api/emojis/autocomplete?q=';
                    } else {
                        endpoint = '/api/accounts/autocomplete?q=';
                    }
                    this._fetchTimer = setTimeout(() => {
                        const controller = new AbortController();
                        this._fetchController = controller;
                        fetch(endpoint + encodeURIComponent(query), {signal: controller.signal})
                            .then(r => r.json())
                            .then(data => {
                                if (id !== this._fetchId) return;
                                this.suggestions = data;
                                this.selectedIndex = -1;
                                this.showSuggestions = data.length > 0;
                                if (this.showSuggestions) this.$nextTick(() => this.placeSuggestions());
                            })
                            .catch(error => {
                                if (id !== this._fetchId || error.name === 'AbortError') return;
                                this.showSuggestions = false;
                            })
                            .finally(() => {
                                if (id === this._fetchId) this._fetchController = null;
                            });
                    }, 250);
                },

                selectSuggestion(index) {
                    const suggestion = this.suggestions[index];
                    if (!suggestion) return;
					let insert;
                    if (this.triggerMode === 'tag') {
                        insert = '#' + suggestion.name;
                    } else if (this.triggerMode === 'emoji') {
                        insert = suggestion.type === 'unicode' ? suggestion.char : (':' + suggestion.shortcode + ':');
                    } else {
                        insert = '@' + suggestion.acct;
                    }
					if (this.editableMode() === 'wysiwyg') {
						this.replaceVisualRange(this._triggerRange, insert + ' ');
					} else {
						const before = this.text.slice(0, this.triggerStart);
						// Spaces and tabs after the token are absorbed, newlines are not:
						// eating one would pull the next line up behind the insertion.
						const after = this.text.slice(this.triggerEnd).replace(/^[^\S\n]+/, '');
						this.text = before + insert + ' ' + after;
					}
					this.closeSuggestions();
                    this.triggerQuery = '';
                    // Focus back on textarea and place cursor after inserted token
                    this.$nextTick(() => {
						const el = this.editorElement();
						if (el) {
							if (el === this.$refs.textarea) {
								const pos = this.triggerStart + insert.length + 1;
								el.setSelectionRange(pos, pos);
							}
							el.focus();
                        }
                    });
				},

				replaceVisualRange(range, insert) {
					if (!range || !this.$refs.visual) return;
					range.deleteContents();
					const inserted = document.createTextNode(insert);
					range.insertNode(inserted);
					range.setStartAfter(inserted);
					range.collapse(true);
					const selection = window.getSelection();
					selection.removeAllRanges();
					selection.addRange(range);
					this.visualHTML = this.$refs.visual.innerHTML;
					this.visualText = this.$refs.visual.innerText;
					this.$refs.visual.focus();
					this.saveSelection();
				},

				onKeydown(event) {
					// Tab nests a list item (Shift+Tab lifts it back out), like every
					// other editor. Only inside a list: elsewhere Tab must keep moving
					// focus, and execCommand('indent') would insert a blockquote.
					if (event.key === 'Tab' && !event.isComposing && !this.showSuggestions && this.editableMode() === 'wysiwyg' && this.selectionInVisual()) {
						const anchor = window.getSelection().anchorNode;
						const element = anchor && (anchor.nodeType === Node.ELEMENT_NODE ? anchor : anchor.parentElement);
						if (element && element.closest('li')) {
							event.preventDefault();
							try { document.execCommand(event.shiftKey ? 'outdent' : 'indent', false, null); } catch (_) {}
							this.onVisualInput();
							return;
						}
					}
					if (event.key === 'Enter' && event.shiftKey && !event.isComposing && this.editableMode() === 'wysiwyg') {
						const selection = window.getSelection();
						if (selection && selection.rangeCount && this.selectionBlockStyle(selection.anchorNode) === 'pre') {
							event.preventDefault();
							let inserted = false;
							try { inserted = document.execCommand('insertLineBreak', false, null); } catch (_) {}
							if (!inserted) {
								const range = selection.getRangeAt(0);
								range.deleteContents();
								const br = document.createElement('br');
								range.insertNode(br);
								range.setStartAfter(br);
								range.collapse(true);
								selection.removeAllRanges();
								selection.addRange(range);
							}
							this.onVisualInput();
							return;
						}
					}
					if (!this.showSuggestions) return;

                    if (event.key === 'ArrowDown') {
                        event.preventDefault();
                        this.selectedIndex = Math.min(this.selectedIndex + 1, this.suggestions.length - 1);
                    } else if (event.key === 'ArrowUp') {
                        event.preventDefault();
                        this.selectedIndex = Math.max(this.selectedIndex - 1, -1);
                    } else if (event.key === 'Enter' || event.key === 'Tab') {
                        if (this.selectedIndex >= 0) {
                            event.preventDefault();
                            this.selectSuggestion(this.selectedIndex);
                        }
                    } else if (event.key === 'Escape') {
                        event.preventDefault();
                        this.closeSuggestions();
                    }
                },

				// Emoji picker. Everything it browses is already in memory:
				// emoji_map.js ships the map and, since it is written in CLDR
				// order, a nine entry index that turns each category into a
				// slice of it. Browsing therefore costs no download and no
				// request, and only the category on screen is ever built.

				onEmojiToggle(event) {
					if (event.target.open && !this._emojiBuilt) this.buildEmojiPicker();
				},

				closeEmoji() {
					const details = this.$refs.emojiGrid && this.$refs.emojiGrid.closest('details');
					if (!details || !details.open) return;
					details.open = false;
					const summary = details.querySelector('summary');
					if (summary) summary.focus();
				},

				buildEmojiPicker() {
					this._emojiBuilt = true;
					this.loadEmojiRecents();
					const tabs = (window.UNICODE_EMOJI_GROUPS || []).map(group => group[0]);
					if (this.emojiRecents.length) tabs.unshift('recent');
					if (!tabs.length) return;
					this.$refs.emojiTabs.innerHTML = tabs.map(slug => this.emojiTabMarkup(slug)).join('');
					this.showEmojiGroup(tabs[0]);
				},

				emojiTabMarkup(slug) {
					return '<button type="button" role="tab" tabindex="-1" aria-selected="false" data-emoji-tab="' +
						escapeMarkup(slug) + '">' + escapeMarkup(this.emojiLabel(slug)) + '</button>';
				},

				// Category names live on the form because the tabs are built
				// here, out of reach of the template's translation function.
				emojiLabel(slug) {
					if (!this._emojiLabels) {
						this._emojiLabels = {};
						(this.$root.dataset.emojiLabels || '').split('|').forEach(pair => {
							const split = pair.indexOf('=');
							if (split > 0) this._emojiLabels[pair.slice(0, split)] = pair.slice(split + 1);
						});
					}
					return this._emojiLabels[slug] || slug;
				},

				// The map is flattened once so a category can be read as a
				// range rather than walked from the start every time.
				emojiEntries() {
					if (!this._emojiEntries) this._emojiEntries = Array.from(window.UNICODE_EMOJI || []);
					return this._emojiEntries;
				},

				showEmojiGroup(slug) {
					this.emojiGroup = slug;
					this.$refs.emojiTabs.querySelectorAll('[data-emoji-tab]').forEach(tab => {
						const active = tab.dataset.emojiTab === slug;
						tab.setAttribute('aria-selected', active ? 'true' : 'false');
						tab.tabIndex = active ? 0 : -1;
					});
					// Recents change under the picker, so that one grid is the
					// only one not worth keeping.
					if (slug !== 'recent' && this._emojiMarkup[slug] === undefined) {
						this._emojiMarkup[slug] = this.emojiGroupMarkup(slug);
					}
					this.$refs.emojiGrid.innerHTML = slug === 'recent' ? this.emojiGroupMarkup(slug) : this._emojiMarkup[slug];
					this.$refs.emojiGrid.scrollTop = 0;
					this.startEmojiRoving();
				},

				emojiGroupMarkup(slug) {
					if (slug === 'recent') return this.emojiRecents.map(entry => this.emojiButtonMarkup(entry)).join('');
					const groups = window.UNICODE_EMOJI_GROUPS || [];
					const at = groups.findIndex(group => group[0] === slug);
					if (at < 0) return '';
					const entries = this.emojiEntries();
					const end = at + 1 < groups.length ? groups[at + 1][1] : entries.length;
					let markup = '';
					for (let i = groups[at][1]; i < end; i++) {
						markup += this.emojiButtonMarkup({shortcode: entries[i][0], char: entries[i][1]});
					}
					return markup;
				},

				// Custom emoji shortcodes and URLs come in over federation, so
				// nothing here is trusted enough to go into markup unescaped.
				emojiButtonMarkup(entry) {
					const shortcode = escapeMarkup(entry.shortcode);
					const body = entry.char ? escapeMarkup(entry.char) :
						'<img src="' + escapeMarkup(entry.url) + '" alt="" loading="lazy">';
					return '<button type="button" tabindex="-1" aria-label=":' + shortcode + ':" data-emoji-shortcode="' +
						shortcode + '"' + (entry.char ? ' data-emoji-char="' + escapeMarkup(entry.char) + '"' : '') +
						'>' + body + '</button>';
				},

				onEmojiTab(event) {
					const tab = event.target.closest('[data-emoji-tab]');
					if (!tab) return;
					this.$refs.emojiSearch.value = '';
					this.showEmojiGroup(tab.dataset.emojiTab);
				},

				onEmojiTabKeydown(event) {
					const tabs = Array.from(this.$refs.emojiTabs.querySelectorAll('[data-emoji-tab]'));
					const at = tabs.indexOf(document.activeElement);
					if (at < 0) return;
					let next;
					if (event.key === 'ArrowRight') next = (at + 1) % tabs.length;
					else if (event.key === 'ArrowLeft') next = (at - 1 + tabs.length) % tabs.length;
					else if (event.key === 'Home') next = 0;
					else if (event.key === 'End') next = tabs.length - 1;
					else return;
					event.preventDefault();
					this.showEmojiGroup(tabs[next].dataset.emojiTab);
					tabs[next].focus();
				},

				// Unicode matches come straight out of the map in memory, so
				// they land before the first keystroke has finished. Only the
				// instance's custom emoji need asking the server, on the same
				// debounce the :shortcode: autocomplete uses.
				onEmojiSearch() {
					const query = this.$refs.emojiSearch.value.trim().toLowerCase().replace(/^:+/, '');
					clearTimeout(this._emojiSearchTimer);
					if (this._emojiSearchController) this._emojiSearchController.abort();
					this._emojiSearchController = null;
					const id = ++this._emojiSearchId;
					if (!query) {
						this.showEmojiGroup(this.emojiGroup || (window.UNICODE_EMOJI_GROUPS || [['smileys']])[0][0]);
						return;
					}

					const starts = [], contains = [];
					for (const [shortcode, char] of this.emojiEntries()) {
						const at = shortcode.indexOf(query);
						if (at === 0) starts.push({shortcode: shortcode, char: char});
						else if (at > 0) contains.push({shortcode: shortcode, char: char});
						if (starts.length >= 60) break;
					}
					const found = starts.concat(contains).slice(0, 60);
					this.$refs.emojiGrid.innerHTML = found.length ?
						found.map(entry => this.emojiButtonMarkup(entry)).join('') :
						'<p class="emoji-empty">' + escapeMarkup(this.$root.dataset.emojiNone || '') + '</p>';
					this.$refs.emojiGrid.scrollTop = 0;
					this.startEmojiRoving();

					this._emojiSearchTimer = setTimeout(() => {
						const controller = new AbortController();
						this._emojiSearchController = controller;
						fetch('/api/emojis/autocomplete?q=' + encodeURIComponent(query), {signal: controller.signal})
							.then(response => response.json())
							.then(data => {
								if (id !== this._emojiSearchId) return;
								const custom = data.filter(entry => entry.type === 'custom' && entry.url);
								if (!custom.length) return;
								const grid = this.$refs.emojiGrid;
								const empty = grid.querySelector('.emoji-empty');
								if (empty) empty.remove();
								// Ahead of the Unicode matches: the instance's
								// own emoji are the ones you cannot find any
								// other way.
								grid.insertAdjacentHTML('afterbegin', custom.map(entry => this.emojiButtonMarkup(entry)).join(''));
								this.startEmojiRoving();
							})
							.catch(() => {})
							.finally(() => { if (id === this._emojiSearchId) this._emojiSearchController = null; });
					}, 250);
				},

				insertEmoji(event) {
					const button = event.target.closest('button[data-emoji-shortcode]');
					if (!button) return;
					const shortcode = button.dataset.emojiShortcode;
					const char = button.dataset.emojiChar || '';
					const insert = char || (':' + shortcode + ':');
					if (this.editableMode() === 'wysiwyg') {
						if (!this.activateVisualSelection()) return;
						this.replaceVisualRange(window.getSelection().getRangeAt(0), insert);
					} else {
						// The textarea keeps its selection through the click,
						// so the emoji lands where the caret was rather than at
						// the end of the post.
						const caret = this.caretOffset();
						this.replaceEditorText(caret, caret, insert);
					}
					const image = button.querySelector('img');
					this.pushEmojiRecent(char ? {shortcode: shortcode, char: char} : {shortcode: shortcode, url: image ? image.getAttribute('src') : ''});
				},

				loadEmojiRecents() {
					try {
						const saved = JSON.parse(localStorage.getItem('littlefedi.composer.emoji.v1') || '[]');
						if (Array.isArray(saved)) {
							this.emojiRecents = saved.filter(entry => entry && typeof entry.shortcode === 'string').slice(0, 30);
						}
					} catch (_) {}
				},

				pushEmojiRecent(entry) {
					this.emojiRecents = [entry].concat(this.emojiRecents.filter(recent => recent.shortcode !== entry.shortcode)).slice(0, 30);
					try { localStorage.setItem('littlefedi.composer.emoji.v1', JSON.stringify(this.emojiRecents)); } catch (_) {}
					// The recents tab only exists once there is something to
					// put in it. The grid is left alone: redrawing it under a
					// pointer that is still clicking would move the target.
					if (this.$refs.emojiTabs && !this.$refs.emojiTabs.querySelector('[data-emoji-tab="recent"]')) {
						this.$refs.emojiTabs.insertAdjacentHTML('afterbegin', this.emojiTabMarkup('recent'));
					}
				},

				// Hundreds of buttons must not each be a tab stop, so only one
				// carries the focus and the arrow keys move it.
				startEmojiRoving() {
					const first = this.$refs.emojiGrid.querySelector('button');
					if (first) first.tabIndex = 0;
				},

				onEmojiGridKeydown(event) {
					const buttons = Array.from(this.$refs.emojiGrid.querySelectorAll('button'));
					const at = buttons.indexOf(document.activeElement);
					if (at < 0) return;
					const columns = Math.max(1, getComputedStyle(this.$refs.emojiGrid).gridTemplateColumns.split(' ').length);
					let next;
					if (event.key === 'ArrowRight') next = at + 1;
					else if (event.key === 'ArrowLeft') next = at - 1;
					else if (event.key === 'ArrowDown') next = at + columns;
					else if (event.key === 'ArrowUp') next = at - columns;
					else if (event.key === 'Home') next = 0;
					else if (event.key === 'End') next = buttons.length - 1;
					else return;
					event.preventDefault();
					if (next < 0 || next >= buttons.length) return;
					buttons[at].tabIndex = -1;
					buttons[next].tabIndex = 0;
					buttons[next].focus();
				}
				};
			});

			Alpine.data('listMemberPicker', function() {
				return {
					query: '',
					results: [],
					async search() {
						const q = this.query.trim().replace(/^@/, '');
						if (q.length < 2) { this.results = []; return; }
						try {
							const resp = await fetch('/api/accounts/autocomplete?following=1&q=' + encodeURIComponent(q));
							this.results = resp.ok ? await resp.json() : [];
						} catch (_) {
							this.results = [];
						}
					}
				};
			});

			Alpine.data('lightbox', function() {
				return {
					shown: false,
					src: '',
					alt: '',
					returnTo: null,
					open(detail) {
						this.shown = true;
						this.src = detail.src;
						this.alt = detail.alt || '';
						this.returnTo = document.activeElement;
						// A modal that leaves the page behind it reachable is
						// only modal for people who can see the backdrop.
						this.outside().forEach(function(el) { el.inert = true; });
						this.$nextTick(() => this.$refs.closeButton.focus());
					},
					close() {
						if (!this.shown) return;
						this.shown = false;
						this.outside().forEach(function(el) { el.inert = false; });
						if (this.returnTo && this.returnTo.isConnected) this.returnTo.focus();
						this.returnTo = null;
					},
					// The backdrop is a direct child of <body>, so its siblings
					// are the whole rest of the page.
					outside() {
						const self = this.$el;
						return Array.prototype.filter.call(document.body.children, (el) => el !== self);
					},
					cycle() {
						// Close is the only control in here, so Tab has nowhere
						// else to land. Escape and the button are the way out.
						this.$refs.closeButton.focus();
					}
				};
			});

			Alpine.data('pushSettings', function() {
				return {
					supported: 'serviceWorker' in navigator && 'PushManager' in window,
					subscribed: false,
					busy: false,
					error: '',
					vapidPublicKey: '',
					serverEndpoint: '',
					csrfToken: '',

					async init() {
						const data = this.$root.dataset;
						this.vapidPublicKey = data.vapidPublicKey || '';
						this.serverEndpoint = data.webPushEndpoint || '';
						this.csrfToken = data.csrfToken || '';
						if (!this.supported) return;
						try {
							const registration = await navigator.serviceWorker.ready;
							const sub = await registration.pushManager.getSubscription();
							this.subscribed = !!sub && sub.endpoint === this.serverEndpoint && this.usesCurrentKey(sub);
						} catch (_) {
							this.error = data.errorInspect || 'Unable to inspect push subscription';
						}
					},

					urlBase64ToUint8Array(base64String) {
						const padding = '='.repeat((4 - base64String.length % 4) % 4);
						const base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
						const raw = window.atob(base64);
						return Uint8Array.from([...raw].map(function(c) { return c.charCodeAt(0); }));
					},

					usesCurrentKey(sub) {
						const current = sub && sub.options && sub.options.applicationServerKey;
						if (!current) return false;
						const got = new Uint8Array(current);
						const want = this.urlBase64ToUint8Array(this.vapidPublicKey);
						return got.length === want.length && got.every(function(value, index) { return value === want[index]; });
					},

					async postJSON(url, body) {
						const res = await fetch(url, {
							method: 'POST',
							credentials: 'same-origin',
							headers: {'Content-Type': 'application/json', 'X-CSRF-Token': this.csrfToken},
							body: JSON.stringify(body || {})
						});
						if (!res.ok) throw new Error('Request failed (' + res.status + ')');
					},

					async subscribe() {
						this.busy = true;
						this.error = '';
						const data = this.$root.dataset;
						try {
							if (Notification.permission === 'denied') throw new Error(data.errorBlocked || 'Notifications are blocked');
							const registration = await navigator.serviceWorker.ready;
							let sub = await registration.pushManager.getSubscription();
							if (sub && !this.usesCurrentKey(sub)) {
								await sub.unsubscribe();
								sub = null;
							}
							if (!sub) {
								sub = await registration.pushManager.subscribe({
									userVisibleOnly: true,
									applicationServerKey: this.urlBase64ToUint8Array(this.vapidPublicKey)
								});
							}
							const json = sub.toJSON();
							await this.postJSON('/settings/push/subscribe', {endpoint: json.endpoint, keys: json.keys});
							this.subscribed = true;
							window.location.replace('/settings#push');
						} catch (e) {
							this.error = e.message || data.errorEnable || 'Unable to enable push notifications';
						} finally {
							this.busy = false;
						}
					},

					async unsubscribe() {
						this.busy = true;
						this.error = '';
						const data = this.$root.dataset;
						try {
							const registration = await navigator.serviceWorker.ready;
							const sub = await registration.pushManager.getSubscription();
							if (sub) await sub.unsubscribe();
							await this.postJSON('/settings/push/unsubscribe');
							this.subscribed = false;
							window.location.replace('/settings#push');
						} catch (e) {
							this.error = e.message || data.errorDisable || 'Unable to disable push notifications';
						} finally {
							this.busy = false;
						}
					}
				};
			});
		});

		// Translated strings for the text this file writes itself, carried on a
		// data-attribute element in base.html. The fallback keeps behaviour
		// unchanged on pages that do not render the full layout.
		function uiString(name, fallback) {
			const holder = document.getElementById('js-strings');
			return (holder && holder.dataset[name]) || fallback;
		}

		// For the few places that assemble markup as a string because building
		// hundreds of nodes one at a time would be slower. Quotes are escaped
		// too: most of what goes through here ends up in an attribute.
		const markupEscapes = {'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'};
		function escapeMarkup(value) {
			return String(value == null ? '' : value).replace(/[&<>"']/g, character => markupEscapes[character]);
		}

		// Everything announced here shares the polite region base.html already
		// keeps for boosted navigations.
		function announce(text) {
			const el = document.getElementById('hx-loading-status');
			if (!el) return;
			// Same string twice in a row is not re-read unless the region is
			// cleared between updates.
			el.textContent = '';
			window.setTimeout(function() { el.textContent = text; }, 50);
		}

		// Native <details> keeps both mobile menus usable without JavaScript.
		// Enhancement closes competing menus and supports outside click/Escape.
		document.addEventListener('toggle', function(event) {
			const opened = event.target.closest && event.target.closest('[data-app-menu]');
			if (!opened || !opened.open) return;
			document.querySelectorAll('[data-app-menu][open]').forEach(function(menu) {
				if (menu !== opened) menu.removeAttribute('open');
			});
		}, true);
		document.addEventListener('click', function(event) {
			if (event.target.closest('[data-app-menu]')) return;
			document.querySelectorAll('[data-app-menu][open]').forEach(function(menu) {
				menu.removeAttribute('open');
			});
		});
        document.addEventListener('keydown', function(event) {
			if (event.key !== 'Escape') return;
			let returnTo;
			document.querySelectorAll('[data-app-menu][open]').forEach(function(menu) {
				if (menu.contains(document.activeElement)) returnTo = menu.firstElementChild;
				menu.removeAttribute('open');
			});
			if (returnTo) {
				event.preventDefault();
				returnTo.focus();
			}
        });

        // Copy controls can arrive later through infinite scrolling.
        document.addEventListener('click', async function(event) {
            const button = event.target.closest('[data-copy-url]');
            if (!button) return;
            event.preventDefault();
            event.stopPropagation();

            const link = new URL(button.dataset.copyUrl, window.location.origin).href;
            let copied = false;
            try {
                await navigator.clipboard.writeText(link);
                copied = true;
            } catch (_) {
                let field;
                try {
                    field = document.body.appendChild(document.createElement('textarea'));
                    field.value = link;
                    field.style.position = 'fixed';
                    field.style.left = '-9999px';
                    field.select();
                    copied = document.execCommand('copy');
                } catch (_) {
                } finally {
                    if (field) field.remove();
                }
            }
            button.focus({preventScroll: true});
            announce(copied ? button.dataset.copiedLabel : button.dataset.copyFailedLabel);
        });

        // Sensitive media stays blurred until the viewer explicitly reveals it.
        // Delegation also covers statuses inserted or replaced by htmx.
        document.addEventListener('click', function(event) {
            const link = event.target.closest('[data-load-image]');
            if (!link) return;

            const image = link.querySelector('img');
            if (image) {
                event.preventDefault();
                window.dispatchEvent(new CustomEvent('open-lightbox', {detail: {src: link.href, alt: image.alt}}));
                return;
            }
            if (!link.dataset.preview) return;
            event.preventDefault();
            const thumbnail = new Image();
            thumbnail.alt = link.dataset.alt || '';
            thumbnail.loading = 'lazy';
            if (link.dataset.width) {
                thumbnail.width = Number(link.dataset.width);
                thumbnail.height = Number(link.dataset.height);
            }
            thumbnail.src = link.dataset.preview;
            link.replaceChildren(thumbnail);
        });

        document.addEventListener('click', function(event) {
            const toggle = event.target.closest('.sensitive-media-toggle');
            if (!toggle) return;

            event.preventDefault();
            event.stopPropagation();
            const wrapper = toggle.closest('[data-sensitive-media]');
            if (!wrapper) return;

            const revealed = wrapper.classList.toggle('revealed');
            toggle.setAttribute('aria-expanded', revealed ? 'true' : 'false');
            const label = toggle.querySelector('span');
            if (label) label.textContent = revealed ? uiString('hideSensitive', 'Hide sensitive media') : uiString('showSensitive', 'Show sensitive media');
        });

        // Make the non-interactive surface of every status and quote card a permalink.
        // Delegation keeps this working for cards added later by htmx scrolling.
        document.addEventListener('click', function(event) {
            if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;

            const card = event.target.closest('[data-status-url]');
            if (!card) return;

            // Preserve links (including local hashtag links), post actions, and
            // controls such as the sensitive-content disclosure.
            // A label counts too: clicking its text drives the control it wraps
            // (the "show more" clamp toggle, a poll option), so navigating away
            // from that click would undo what the reader just asked for.
            if (event.target.closest('a, button, input, label, select, textarea, summary, [contenteditable], [role="button"]')) return;

            const selection = window.getSelection();
            if (selection && !selection.isCollapsed) return;

            saveTimelinePosition();

            // Use native navigation so htmx does not serialize an arbitrarily
            // large infinite-scroll DOM into localStorage before showing the
            // post. Browser history/bfcache and our saved anchor handle Back.
            window.location.assign(card.dataset.statusUrl);
        });

        // A group can straddle two infinite-scroll pages. Keep the first
        // (newest) card and discard any later rendering of the same original.
        function deduplicateTimelineStatuses() {
            const seen = new Set();
            document.querySelectorAll('#timeline .status[data-original-status-id]').forEach(function(card) {
                const id = card.dataset.originalStatusId;
                if (seen.has(id)) {
                    card.remove();
                } else {
                    seen.add(id);
                }
            });
        }
        document.addEventListener('DOMContentLoaded', deduplicateTimelineStatuses);
        document.addEventListener('htmx:afterSettle', deduplicateTimelineStatuses);

        // Infinite scroll only ever appends, so a long session keeps every card
        // it has rendered. That is memory on a phone plus a longer walk for
        // every pass that queries the whole timeline. Drop the oldest cards once
        // they are well above the viewport and put the scroll offset back, so
        // nothing shifts under the reader. Scrolling far enough back shows fewer
        // old posts, which is the trade every long feed makes.
        // Limit by card count. A pixel budget would be fairer when tall
        // media makes 300 cards mean very different amounts of DOM.
        const timelineCardLimit = 300;
        function pruneTimeline() {
            const timeline = document.getElementById('timeline');
            // A restore is walking backwards through pages looking for a saved
            // anchor. Removing cards underneath it would send it looking forever.
            if (!timeline || pendingRestore) return;

            const cards = timeline.querySelectorAll('.status');
            const excess = cards.length - timelineCardLimit;
            if (excess < 1) return;

            let doomed = 0;
            // Only cards that are entirely above the viewport. Anything on
            // screen, and the anchor a Back restore looks for, has to stay.
            while (doomed < excess && cards[doomed].getBoundingClientRect().bottom < 0) doomed++;
            if (!doomed) return;

            const keep = cards[doomed];
            const before = keep.getBoundingClientRect().top;
            for (let i = 0; i < doomed; i++) cards[i].remove();
            window.scrollBy(0, keep.getBoundingClientRect().top - before);
        }
        document.addEventListener('htmx:afterSettle', pruneTimeline);

        function fitNavs() {
            document.querySelectorAll('.timeline-nav').forEach(function(nav) {
                nav.classList.remove('timeline-nav-compact');
                const links = nav.querySelectorAll('a');
                if (!links.length || !nav.offsetWidth) return;
                const wrapped = links[links.length - 1].offsetTop > links[0].offsetTop;
                if (wrapped || nav.scrollWidth > nav.clientWidth) nav.classList.add('timeline-nav-compact');
            });
            document.querySelectorAll('.desktop-nav').forEach(function(nav) {
                nav.classList.remove('desktop-nav-compact');
                if (nav.scrollWidth > nav.clientWidth) nav.classList.add('desktop-nav-compact');
            });
        }
        window.addEventListener('resize', fitNavs);
        document.addEventListener('DOMContentLoaded', fitNavs);
        document.addEventListener('htmx:afterSettle', fitNavs);
        if (document.fonts) document.fonts.ready.then(fitNavs);

        let timelineComposeReturnY = null;
        function clearTimelineComposeReturnY() {
            timelineComposeReturnY = null;
            updateTimelineFloatingActions();
        }
        function updateTimelineFloatingActions() {
            const actions = document.querySelector('[data-timeline-floating-actions]');
            if (!actions) return;
            const hasReturn = timelineComposeReturnY !== null;
            const showTimelineActions = window.scrollY > 240 && !hasReturn;
            const topButton = actions.querySelector('[data-timeline-top]');
            const composeButton = actions.querySelector('[data-timeline-compose]');
            const returnButton = actions.querySelector('[data-timeline-return]');
            actions.hidden = !showTimelineActions && !hasReturn;
            if (topButton) topButton.hidden = !showTimelineActions;
            if (composeButton) composeButton.hidden = !showTimelineActions;
            if (returnButton) returnButton.hidden = !hasReturn;
        }
        function timelineScrollBehavior() {
            return window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth';
        }
        window.addEventListener('scroll', updateTimelineFloatingActions, {passive: true});
        document.addEventListener('DOMContentLoaded', updateTimelineFloatingActions);
        document.addEventListener('htmx:afterSettle', updateTimelineFloatingActions);
        document.addEventListener('htmx:beforeRequest', function(event) {
            const source = event.detail && event.detail.elt;
            if (!event.detail || !event.detail.boosted) return;
            if (!(source && source.closest('.composer'))) clearTimelineComposeReturnY();
            cancelTimelineRestore();
        });
        window.addEventListener('pagehide', clearTimelineComposeReturnY);
        document.addEventListener('htmx:historyRestore', clearTimelineComposeReturnY);
        window.addEventListener('popstate', clearTimelineComposeReturnY);
        document.addEventListener('click', function(event) {
            const jump = event.target.closest('[data-timeline-top], [data-timeline-compose]');
            if (jump) {
                const composer = document.querySelector('.timeline .composer');
                if (!composer) return;
                const collapsedComposer = composer.closest('.timeline-composer');
                if (collapsedComposer) collapsedComposer.open = true;
                timelineComposeReturnY = window.scrollY;
                updateTimelineFloatingActions();
                composer.scrollIntoView({block: 'start', behavior: timelineScrollBehavior()});
                if (jump.matches('[data-timeline-compose]')) {
                    const textarea = composer.querySelector('textarea');
                    if (textarea) textarea.focus({preventScroll: true});
                }
                return;
            }
            if (event.target.closest('[data-timeline-return]')) {
                const y = timelineComposeReturnY;
                if (y === null) return;
                clearTimelineComposeReturnY();
                window.scrollTo({top: y, behavior: timelineScrollBehavior()});
                return;
            }
        });

        // Boost/favourite/bookmark/pin replace the whole card, so the control
        // that was just pressed is destroyed and focus falls back to <body>.
        // The form action survives the swap, so it identifies the replacement.
        // Refocusing it also makes the screen reader read the new state, which
        // is the only confirmation these actions ever give.
        (function() {
            let pendingAction = null;
            document.addEventListener('htmx:beforeRequest', function() {
                pendingAction = null;
                const active = document.activeElement;
                if (!active || !active.classList || !active.classList.contains('action-submit')) return;
                const form = active.closest('form');
                if (form) pendingAction = form.getAttribute('action');
            });
            document.addEventListener('htmx:afterSettle', function() {
                if (!pendingAction) return;
                const action = pendingAction;
                pendingAction = null;
                const submit = document.querySelector('form[action="' + action + '"] .action-submit');
                if (submit) submit.focus({preventScroll: true});
                // Deleting removes the card outright, so there is nothing left
                // to focus and nothing to read.
                else if (action.endsWith('/delete')) announce(uiString('postDeleted', 'Post deleted.'));
            });
            // A failed request never settles, so without this the button that
            // was pressed stays pending and the next settle refocuses for it.
            document.addEventListener('htmx:afterRequest', function(event) {
                if (event.detail && event.detail.successful !== true) pendingAction = null;
            });
        })();

        // Alpine.js does not re-initialize after htmx swaps. Re-init on
        // boosted navigations (the body is brand-new) and partial swaps
        // (only the target subtree).
        document.addEventListener('htmx:afterSettle', function(event) {
            if (typeof Alpine === 'undefined') return;
            if (event.detail && event.detail.boosted) {
                Alpine.initTree(document.body);
            } else if (event.detail && event.detail.target) {
                Alpine.initTree(event.detail.target);
            }
        });

        document.addEventListener('htmx:afterSettle', function(event) {
            const source = event.detail && event.detail.requestConfig && event.detail.requestConfig.elt;
            if (!(source && source.dataset.replyComposer)) return;
            const composer = document.getElementById(source.dataset.replyComposer);
            if (!composer) return;
            composer.scrollIntoView({block: 'center', behavior: 'auto'});
            requestAnimationFrame(function() {
                const textarea = composer.querySelector('textarea');
                if (textarea) {
                    textarea.focus({preventScroll: true});
                    textarea.setSelectionRange(textarea.value.length, textarea.value.length);
                }
            });
        });

        // A thread is opened for one specific post, even when that post is a
        // reply with a long chain of ancestors. Put that post in view on first
        // paint and keep it at the same screen position while background thread
        // completion inserts or replaces ancestors above it.
        let threadFocusTop = null;
        function revealThreadFocus() {
            const thread = document.querySelector('[data-thread-focus]');
            if (!thread) return;
            if (location.hash.startsWith('#status-interactions-')) {
                const interactions = document.getElementById(location.hash.slice(1));
                if (interactions) interactions.scrollIntoView({block: 'start', behavior: 'auto'});
                return;
            }
            if (new URLSearchParams(window.location.search).get('reply') === '1') {
                const composer = thread.querySelector('#composer');
                if (!composer) return;
                const disclosure = composer.closest('details');
                if (disclosure) disclosure.open = true;
                composer.scrollIntoView({block: 'start', behavior: 'auto'});
                requestAnimationFrame(function() {
                    const textarea = composer.querySelector('textarea');
                    if (textarea) {
                        textarea.focus({preventScroll: true});
                        textarea.setSelectionRange(textarea.value.length, textarea.value.length);
                    }
                });
                return;
            }
            const focus = document.getElementById(thread.dataset.threadFocus);
			if (focus) focus.scrollIntoView({block: 'start', behavior: 'auto'});
        }
        document.addEventListener('DOMContentLoaded', revealThreadFocus);
        document.addEventListener('htmx:beforeSwap', function(event) {
            if (!event.detail || !event.detail.target || event.detail.target.id !== 'thread-replies') return;
            const focus = document.getElementById('thread-focus');
            if (focus) threadFocusTop = focus.getBoundingClientRect().top;
        });
        document.addEventListener('htmx:afterSettle', function(event) {
            if (threadFocusTop === null || !event.detail || !event.detail.target || event.detail.target.id !== 'thread-replies') return;
            const focus = document.getElementById('thread-focus');
            if (focus) window.scrollBy(0, focus.getBoundingClientRect().top - threadFocusTop);
            threadFocusTop = null;
        });
        document.addEventListener('htmx:afterRequest', function(event) {
            if (event.detail && event.detail.successful !== true) threadFocusTop = null;
        });

        // Save independently of htmx's history cache. This also covers native
        // navigation and htmx cache misses.
        function timelinePositionKey() {
            return 'littlefedi:timeline-position:' + location.pathname + location.search;
        }

        function saveTimelinePosition() {
            const timeline = document.getElementById('timeline');
            if (!timeline) return;

            const cards = Array.from(timeline.querySelectorAll('.status[id]'));
            const anchor = cards.find(function(card) {
                return card.getBoundingClientRect().bottom > 0;
            });
            if (!anchor) return;

            try {
                sessionStorage.setItem(timelinePositionKey(), JSON.stringify({
                    id: anchor.id,
                    offset: anchor.getBoundingClientRect().top
                }));
            } catch (_) {}
        }

        // Timestamp/reply links bypass the card-surface handler above.
        document.addEventListener('click', function(event) {
            if (event.target.closest('#timeline .status a[href]')) saveTimelinePosition();
        }, true);

        let timelineRestoreGeneration = 0;
        let pendingRestore = null;

        function cancelTimelineRestore() {
            timelineRestoreGeneration++;
            pendingRestore = null;
        }

        ['wheel', 'touchstart', 'pointerdown', 'keydown'].forEach(function(type) {
            window.addEventListener(type, cancelTimelineRestore, {passive: true});
        });

        function discardTimelinePosition(key) {
            try { sessionStorage.removeItem(key); } catch (_) {}
        }

        // Older batches load below the viewport. Once the saved card arrives,
        // one instant adjustment puts it back at the same screen position.
        function attemptRestore() {
            const restore = pendingRestore;
            if (!restore || restore.generation !== timelineRestoreGeneration) return;
            if (restore.key !== timelinePositionKey()) {
                cancelTimelineRestore();
                return;
            }

            const anchor = document.getElementById(restore.id);
            if (anchor) {
                const delta = anchor.getBoundingClientRect().top - restore.offset;
                if (Math.abs(delta) > 1) window.scrollBy({top: delta, behavior: 'instant'});
                pendingRestore = null;
                return;
            }

            const pages = document.querySelectorAll('[hx-get^="/partial/timeline/"]');
            const next = pages[pages.length - 1];
            if (!next || next === restore.trigger) {
                discardTimelinePosition(restore.key);
                pendingRestore = null;
                return;
            }
            restore.trigger = next;
            if (!next.classList.contains('htmx-request')) htmx.trigger(next, 'timeline-restore');
        }

        function restoreTimelinePosition() {
            if (!document.getElementById('timeline')) return;
            cancelTimelineRestore();
            const key = timelinePositionKey();
            let saved;
            try {
                saved = JSON.parse(sessionStorage.getItem(key));
            } catch (_) {
                discardTimelinePosition(key);
                return;
            }
            if (!saved) return;
            if (typeof saved.id !== 'string' || !saved.id.startsWith('status-') || !Number.isFinite(saved.offset)) {
                discardTimelinePosition(key);
                return;
            }

            const generation = ++timelineRestoreGeneration;
            pendingRestore = {id: saved.id, offset: saved.offset, key: key, generation: generation, trigger: null};
            window.setTimeout(attemptRestore, 0);
        }

        document.addEventListener('htmx:historyRestore', restoreTimelinePosition);
        window.addEventListener('popstate', function() {
            window.setTimeout(restoreTimelinePosition, 0);
        });
        window.addEventListener('pageshow', function(event) {
            const navigation = performance.getEntriesByType && performance.getEntriesByType('navigation')[0];
            if (event.persisted || (navigation && navigation.type === 'back_forward')) {
                restoreTimelinePosition();
            }
        });

        document.addEventListener('htmx:afterSettle', function(event) {
            if (pendingRestore && event.detail && event.detail.elt === pendingRestore.trigger) attemptRestore();
        });
        document.addEventListener('htmx:afterRequest', function(event) {
            if (pendingRestore && event.detail && event.detail.elt === pendingRestore.trigger && event.detail.successful !== true) {
                cancelTimelineRestore();
            }
        });

        // Use the actual history entry so the browser/htmx can restore the
        // previous timeline and its scroll position. The href remains the
        // no-JS (and empty-history) fallback.
        document.addEventListener('click', function(event) {
            const backLink = event.target.closest('.back-link');
            if (!backLink || event.defaultPrevented || event.button !== 0 ||
                event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
            if (window.history.length > 1) {
                event.preventDefault();
                window.history.back();
            }
        });

        // CSP-safe replacement for inline onsubmit confirmation handlers.
        // Forms still submit normally when JavaScript is unavailable.
        document.addEventListener('submit', function(event) {
            const form = event.target.closest('form[data-confirm]');
            if (form && !window.confirm(form.dataset.confirm)) {
                event.preventDefault();
                event.stopPropagation();
            }
        }, true);
        document.addEventListener('click', function(event) {
            const link = event.target.closest('a[data-confirm]');
            if (link && !window.confirm(link.dataset.confirm)) {
                event.preventDefault();
                event.stopPropagation();
            }
        }, true);
        document.addEventListener('click', function(event) {
            const cancel = event.target.closest('[data-inline-reply-cancel]');
            if (!cancel) return;
            const wrapper = cancel.closest('[data-inline-reply-composer]');
            if (!wrapper) return;
            event.preventDefault();
            wrapper.remove();
        }, true);
        document.addEventListener('error', function(event) {
            if (event.target.matches && event.target.matches('img[data-remove-on-error]')) event.target.remove();
        }, true);

        // Boosted requests (nav links, composer post, sign-out, search) swap the
        // whole page, so a slow/high-latency response can look like the click did
        // nothing. Surface a top progress bar + a screen-reader announcement for
        // the duration of any in-flight boosted request. Non-boosted requests
        // (like/boost/bookmark, infinite scroll, background previews) are left
        // alone since those already have their own local feedback.
        (function() {
            let inFlight = 0;
            function setStatus(text) {
                const el = document.getElementById('hx-loading-status');
                if (el) el.textContent = text;
            }
            document.addEventListener('htmx:beforeRequest', function(event) {
                if (!event.detail || !event.detail.boosted) return;
                inFlight++;
                document.documentElement.classList.add('hx-loading');
                setStatus(uiString('loading', 'Loading…'));
            });
            function done(event) {
                if (!event.detail || !event.detail.boosted) return;
                inFlight = Math.max(0, inFlight - 1);
                if (inFlight === 0) {
                    document.documentElement.classList.remove('hx-loading');
                    setStatus('');
                }
            }
            // htmx fires afterRequest before sendError, sendAbort and timeout
            // alike, so this one listener covers every way a request can end.
            // Counting sendError as well dropped the bar while a second boosted
            // request was still running.
            document.addEventListener('htmx:afterRequest', done);
        })();

        // Boosted requests silently drop non-2xx response bodies (htmx's default
        // responseHandling never swaps error responses in), so a failed compose
        // upload, poll vote, etc. would look like nothing happened. Show
        // the server's error text in a dismissable toast instead.
        (function() {
            // Looked up per call, not once here: app.js runs in <head> without
            // defer, so at script-evaluation time the body (and the toast) does
            // not exist yet and an up-front lookup silently disabled all of this.
            let hideTimer;
            function showToast(text) {
                const toast = document.getElementById('hx-error-toast');
                if (!toast) return;
                toast.textContent = text || uiString('requestFailed', 'Request failed.');
                toast.hidden = false;
                clearTimeout(hideTimer);
                hideTimer = setTimeout(() => { toast.hidden = true; }, 6000);
            }
            document.addEventListener('click', function(event) {
                if (event.target && event.target.id === 'hx-error-toast') event.target.hidden = true;
            });
            document.addEventListener('htmx:responseError', function(event) {
                // Handlers that mean the message for the reader answer in plain
                // text. Anything else is an error page, and pouring its markup
                // into an aria-live region helps nobody.
                const xhr = event.detail && event.detail.xhr;
                const type = (xhr && xhr.getResponseHeader('Content-Type')) || '';
                const body = (xhr && xhr.responseText) || '';
                showToast(type.indexOf('text/plain') === 0 && body.length < 300 ? body : '');
            });
            document.addEventListener('htmx:sendError', function() {
                showToast(uiString('networkError', 'Network error, please try again.'));
            });
        })();

        // Status cards are cached for much longer than their relative time
        // labels stay accurate. Keep only the absolute timestamp in cached HTML
        // and derive the changing label in the browser.
        (function() {
            if (typeof Intl.RelativeTimeFormat !== 'function') return;
            const locale = document.documentElement.lang || undefined;
            const relative = new Intl.RelativeTimeFormat(locale, {numeric: 'auto', style: 'narrow'});
            const absolute = new Intl.DateTimeFormat(locale, {dateStyle: 'medium'});
            const exact = new Intl.DateTimeFormat(locale, {dateStyle: 'medium', timeStyle: 'short'});

            // Some people would rather read the date than "6m ago". The choice
            // lives in this browser: the labels are rewritten here anyway, and
            // the cached HTML has to stay the same for everyone.
            const absoluteKey = 'littlefedi.time.absolute.v1';
            let alwaysAbsolute = false;
            try { alwaysAbsolute = localStorage.getItem(absoluteKey) === '1'; } catch (_) {}

            function updateRelativeTime(el) {
                const date = new Date(el.dateTime);
                if (isNaN(date.getTime())) return;
                if (alwaysAbsolute) { el.textContent = exact.format(date); return; }
                const seconds = (date.getTime() - Date.now()) / 1000;
                const age = Math.abs(seconds);
                if (age < 60) el.textContent = relative.format(0, 'second');
                else if (age < 3600) el.textContent = relative.format(Math.trunc(seconds / 60), 'minute');
                else if (age < 86400) el.textContent = relative.format(Math.trunc(seconds / 3600), 'hour');
                else if (age < 604800) el.textContent = relative.format(Math.trunc(seconds / 86400), 'day');
                else el.textContent = absolute.format(date);
            }

            function updateRelativeTimes(root) {
                if (root.matches && root.matches('[data-relative-time]')) updateRelativeTime(root);
                root.querySelectorAll('[data-relative-time]').forEach(updateRelativeTime);
            }

            document.addEventListener('DOMContentLoaded', function() {
                updateRelativeTimes(document);
                const toggle = document.getElementById('absolute-times');
                if (!toggle) return;
                toggle.checked = alwaysAbsolute;
                toggle.addEventListener('change', function() {
                    alwaysAbsolute = toggle.checked;
                    try { localStorage.setItem(absoluteKey, alwaysAbsolute ? '1' : '0'); } catch (_) {}
                    updateRelativeTimes(document);
                });
            });
            document.addEventListener('htmx:afterSettle', function(event) {
                updateRelativeTimes(event.target);
            });
            document.addEventListener('littlefedi:timeline-update', function() { updateRelativeTimes(document); });
            document.addEventListener('visibilitychange', function() {
                if (!document.hidden) updateRelativeTimes(document);
            });
            setInterval(function() {
                if (!document.hidden) updateRelativeTimes(document);
            }, 30000);
        })();

        // Fills every hidden timezone-offset field on the page, so the server can
        // turn a timezone-less datetime-local value into the instant the viewer
        // actually meant. Without JS the fields stay empty and the server reads
        // those values as its own local time, which is the documented fallback.
        function fillTimezoneOffsets(root) {
            const offset = new Date().getTimezoneOffset();
            (root || document).querySelectorAll('.js-tz-offset').forEach(function(field) {
                field.value = offset;
            });
        }

        // Progressive enhancement only: the server renders timestamps in UTC
        // (labelled, cacheable, correct with no JS). Here we swap the visible
        // text for the viewer's locale and timezone, reusing the locale's own
        // translated sentence (data-localtime-template) so word order/wording
        // stays correct without duplicating any translation in JS.
        (function() {
            function pad(n) { return String(n).padStart(2, '0'); }

            function localizeTimes(root) {
                root.querySelectorAll('[data-localtime-utc]').forEach(function(el) {
                    const date = new Date(el.dataset.localtimeUtc);
                    if (isNaN(date)) return;
                    const local = new Intl.DateTimeFormat(undefined, {dateStyle: 'medium', timeStyle: 'short'}).format(date);
                    // A datetime-local input carries the value itself; anything
                    // else carries a sentence with a %s placeholder.
                    if (el.tagName === 'INPUT') {
                        el.value = date.getFullYear() + '-' + pad(date.getMonth() + 1) + '-' + pad(date.getDate()) +
                            'T' + pad(date.getHours()) + ':' + pad(date.getMinutes());
                        return;
                    }
                    const tmpl = el.dataset.localtimeTemplate;
                    const textEl = el.querySelector('.localtime-text');
                    if (!tmpl || !textEl) return;
                    textEl.textContent = tmpl.replace('%s', local);
                });
            }

            function enhance(root) {
                localizeTimes(root);
                fillTimezoneOffsets(root);
            }

            document.addEventListener('DOMContentLoaded', function() { enhance(document); });
            document.addEventListener('htmx:afterSettle', function(event) {
                enhance(event.detail && event.detail.target ? event.detail.target : document);
            });
        })();

        (function() {
            const timers = new WeakMap();

            function reactionButton(value, label, image, pressed) {
                const button = document.createElement('button');
                button.type = 'button';
                button.dataset.reaction = value;
                button.setAttribute('aria-pressed', pressed ? 'true' : 'false');
                button.setAttribute('aria-label', label.replace('__EMOJI__', value));
                if (image) {
                    const img = document.createElement('img');
                    img.src = image;
                    img.alt = value;
                    button.appendChild(img);
                } else {
                    button.textContent = value;
                }
                return button;
            }

            function renderReactionGrid(details, query, custom) {
                const grid = details.querySelector('[data-reaction-grid]');
                if (!grid) return;
                const fragment = document.createDocumentFragment();
                const selected = new Set();
                details.closest('.status').querySelectorAll('.status-reactions button[aria-pressed="true"]').forEach(function(button) {
                    const input = button.form && button.form.elements.emoji;
                    if (input) selected.add(input.value);
                });
                Array.from(window.UNICODE_EMOJI || []).forEach(function(entry) {
                    if (!query || entry[0].includes(query) || entry[1] === query) {
                        const pressed = selected.has(entry[1]);
                        const label = pressed ? details.dataset.reactionRemoveLabel : details.dataset.reactionAddLabel;
                        fragment.appendChild(reactionButton(entry[1], label, '', pressed));
                    }
                });
                (custom || []).forEach(function(entry) {
                    const value = ':' + entry.shortcode + '@' + entry.domain + ':';
                    const pressed = selected.has(value);
                    const label = pressed ? details.dataset.reactionRemoveLabel : details.dataset.reactionAddLabel;
                    fragment.appendChild(reactionButton(value, label, entry.url, pressed));
                });
                grid.replaceChildren(fragment);
                if (details.open) requestAnimationFrame(function() { positionReactionMenu(details); });
            }

            function positionReactionMenu(details) {
                const menu = details.querySelector('.reaction-menu');
                const summary = details.querySelector(':scope > summary');
                if (!menu || !summary || !details.open) return;
                const anchor = summary.getBoundingClientRect();
                const header = document.querySelector('.header');
                const headerBox = header && header.getBoundingClientRect();
                const topBar = headerBox && headerBox.height < window.innerHeight / 2 && headerBox.left < anchor.right && headerBox.right > anchor.left;
                const topEdge = Math.max(8, topBar ? headerBox.bottom + 8 : 8);
                const mobileNav = document.querySelector('.mobile-bottom-nav');
                const mobileNavBox = mobileNav && mobileNav.getBoundingClientRect();
                const bottomEdge = mobileNavBox && mobileNavBox.height && mobileNavBox.top > anchor.bottom ? mobileNavBox.top - 8 : window.innerHeight - 8;
                const gap = 8;
                const above = anchor.top - topEdge - gap;
                const below = bottomEdge - anchor.bottom - gap;
                const height = menu.scrollHeight;
                const openBelow = height <= below || (height > above && below > above);
                const available = Math.max(0, openBelow ? below : above);
                const width = menu.offsetWidth;
                const center = anchor.left + anchor.width / 2;
                const shift = center - width / 2 < 8 ? 8 - (center - width / 2) : center + width / 2 > window.innerWidth - 8 ? window.innerWidth - 8 - (center + width / 2) : 0;

                menu.style.position = 'absolute';
                menu.style.left = 'calc(50% + ' + shift + 'px)';
                menu.style.transform = 'translateX(-50%)';
                menu.style.maxHeight = available + 'px';
                menu.style.overflowY = 'auto';
                if (openBelow) {
                    menu.style.top = 'calc(100% + ' + gap + 'px)';
                    menu.style.bottom = 'auto';
                } else {
                    menu.style.top = 'auto';
                    menu.style.bottom = 'calc(100% + ' + gap + 'px)';
                }
            }

            document.addEventListener('toggle', function(event) {
                const details = event.target.closest && event.target.closest('[data-reaction-picker]');
                if (!details || !details.open) return;
                document.querySelectorAll('[data-reaction-picker][open]').forEach(function(other) {
                    if (other !== details) other.removeAttribute('open');
                });
                if (!details.dataset.built) {
                    details.dataset.built = 'true';
                    renderReactionGrid(details, '', []);
                } else {
                    requestAnimationFrame(function() { positionReactionMenu(details); });
                }
            }, true);

            function repositionOpenReactionMenus() {
                document.querySelectorAll('[data-reaction-picker][open]').forEach(positionReactionMenu);
            }

            // Capture-phase scroll fires for every scrolling element on the
            // page, and the pass above queries the whole document each time, so
            // on a long timeline it costs a frame per flick even with no menu
            // open. Bind it only while one is. toggle covers every open and
            // close, including the ones script makes by dropping the attribute.
            let repositionBound = false;
            function bindReactionReposition(bound) {
                if (bound === repositionBound) return;
                repositionBound = bound;
                const listen = bound ? 'addEventListener' : 'removeEventListener';
                window[listen]('resize', repositionOpenReactionMenus);
                window[listen]('scroll', repositionOpenReactionMenus, true);
            }
            document.addEventListener('toggle', function() {
                bindReactionReposition(!!document.querySelector('[data-reaction-picker][open]'));
            }, true);

            document.addEventListener('click', function(event) {
                if (event.target.closest && event.target.closest('[data-reaction-picker]')) return;
                document.querySelectorAll('[data-reaction-picker][open]').forEach(function(details) { details.removeAttribute('open'); });
            });

            document.addEventListener('input', function(event) {
                const input = event.target.closest && event.target.closest('[data-reaction-input]');
                if (!input) return;
                const details = input.closest('[data-reaction-picker]');
                const query = input.value.trim().toLowerCase().replace(/^:+|:+$/g, '');
                clearTimeout(timers.get(input));
                timers.set(input, setTimeout(function() {
                    if (!query) {
                        renderReactionGrid(details, '', []);
                        return;
                    }
                    fetch('/api/emojis/autocomplete?q=' + encodeURIComponent(query))
                        .then(function(response) { return response.ok ? response.json() : []; })
                        .then(function(entries) { renderReactionGrid(details, query, entries.filter(function(entry) { return entry.type === 'custom'; })); })
                        .catch(function() { renderReactionGrid(details, query, []); });
                }, 180));
            });

            document.addEventListener('click', function(event) {
                const button = event.target.closest && event.target.closest('[data-reaction]');
                if (!button) return;
                const form = button.closest('[data-reaction-form]');
                form.querySelector('[data-reaction-input]').value = button.dataset.reaction;
                form.requestSubmit();
            });
        })();

        // Progressive enhancement only: pages work fully without this. Registers
        // the offline shell / installability service worker; no-op where SW or
        // JS is unavailable.
        if ('serviceWorker' in navigator) {
            window.addEventListener('load', () => {
                navigator.serviceWorker.register('/sw.js').catch(() => {});
            });
        }
