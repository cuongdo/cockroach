import * as React from "react";
import {runVisualization, viewWidth, viewHeight} from "../js/sim/index";

/**
 * TODO: fill this comment in
 */
export default class ClusterViz extends React.Component<{}, {}> {
    modelElem: Element;

    static title() {
        return "Explain for Distributed SQL";
    }

    componentDidMount() {
        // After mounting, turn the DOM element over to the visualization code.
        runVisualization(this.modelElem);
    }

    render() {
        let style = {
            height: viewHeight,
            width: viewWidth,
        };
        return <div className="cluster-viz" style={style} ref={(elem) => this.modelElem = elem}/>;
    }
}
