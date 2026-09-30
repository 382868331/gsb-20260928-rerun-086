import { Component, type ReactNode } from 'react';

interface State {
	error: Error | null;
}

// ErrorBoundary keeps the研发台 shell alive when a single render path throws
// (e.g. an unexpected empty payload). Instead of a blank page the user gets
// the error text and a one-click reset.
export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
	state: State = { error: null };

	static getDerivedStateFromError(error: Error): State {
		return { error };
	}

	componentDidCatch(error: Error) {
		console.error('render error caught by boundary:', error);
	}

	reset = () => this.setState({ error: null });

	render() {
		if (this.state.error) {
			return (
				<div className="fatal boundary">
					<h2>面板渲染异常（已隔离，未白屏）</h2>
					<pre className="mono small">{String(this.state.error.stack ?? this.state.error)}</pre>
					<button onClick={this.reset}>重置面板</button>
				</div>
			);
		}
		return this.props.children;
	}
}
